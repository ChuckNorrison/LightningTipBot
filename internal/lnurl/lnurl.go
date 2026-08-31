package lnurl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/time/rate"

	"github.com/eko/gocache/store"
	tb "gopkg.in/lightningtipbot/telebot.v3"

	"github.com/ChuckNorrison/LightningTipBot/internal"
	"github.com/ChuckNorrison/LightningTipBot/internal/api"
	"github.com/ChuckNorrison/LightningTipBot/internal/storage"
	"gorm.io/gorm"

	"github.com/ChuckNorrison/LightningTipBot/internal/lnbits"
	"github.com/ChuckNorrison/LightningTipBot/internal/runtime"
	"github.com/ChuckNorrison/LightningTipBot/internal/telegram"
	"github.com/fiatjaf/go-lnurl"
	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

const (
	PayRequestTag            = "payRequest"
	Endpoint                 = ".well-known/lnurlp"
	MinSendable              = 1000 // mSat
	MaxSendable              = 250_000_000
	CommentAllowed           = 140
	MaxInvoicesPerUserPerMin = 10
	MaxInvoicesPerIPPerMin   = 30
)

type invoiceLimiter struct {
	mu   sync.Mutex
	keys map[string]*rate.Limiter
	r    rate.Limit
	b    int
}

func newInvoiceLimiter(perMin int) *invoiceLimiter {
	return &invoiceLimiter{
		keys: make(map[string]*rate.Limiter),
		r:    rate.Limit(float64(perMin) / 60.0),
		b:    perMin,
	}
}

func (l *invoiceLimiter) allow(key string) bool {
	if key == "" {
		key = "unknown"
	}
	l.mu.Lock()
	lim, ok := l.keys[key]
	if !ok {
		lim = rate.NewLimiter(l.r, l.b)
		l.keys[key] = lim
	}
	l.mu.Unlock()
	return lim.Allow()
}

var (
	lnurlUserLimiter = newInvoiceLimiter(MaxInvoicesPerUserPerMin)
	lnurlIPLimiter   = newInvoiceLimiter(MaxInvoicesPerIPPerMin)
)

func sanitizeComment(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > CommentAllowed {
		s = s[:CommentAllowed]
	}
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, _ := strings.Cut(r.RemoteAddr, ":")
	if host == "" {
		return r.RemoteAddr
	}
	return host
}

func lnurlError(reason string) *lnurl.LNURLPayValues {
	return &lnurl.LNURLPayValues{
		LNURLResponse: lnurl.LNURLResponse{Status: api.StatusError, Reason: reason},
	}
}

func userCannotReceiveLNURL(user *lnbits.User) bool {
	if user == nil {
		return true
	}
	if user.Banned {
		return true
	}
	if user.Wallet == nil {
		return true
	}
	if user.Wallet.Inkey == "" {
		return true
	}
	key := strings.ToLower(strings.TrimSpace(user.Wallet.Adminkey))
	return strings.HasPrefix(key, "banned")
}

func invalidUser() (*lnurl.LNURLPayValues, error) {
	return &lnurl.LNURLPayValues{
		LNURLResponse: lnurl.LNURLResponse{
			Status: api.StatusError,
			Reason: "Invalid user.",
		},
	}, fmt.Errorf("invalid or banned user")
}

const DefaultMaxSendableSat = 250_000

func maxSendableMsat() int64 {
    sat := internal.Configuration.Bot.LNURLMaxSendableSat
    if sat <= 0 {
        sat = DefaultMaxSendableSat
    }
    return sat * 1000
}

type Invoice struct {
	*telegram.Invoice
	Comment   string       `json:"comment"`
	User      *lnbits.User `json:"user"`
	CreatedAt time.Time    `json:"created_at"`
	Paid      bool         `json:"paid"`
	PaidAt    time.Time    `json:"paid_at"`
	From      string       `json:"from"`
}
type Lnurl struct {
	telegram         *tb.Bot
	c                *lnbits.Client
	database         *gorm.DB
	callbackHostname *url.URL
	buntdb           *storage.DB
	WebhookServer    string
	cache            telegram.Cache
}

func New(bot *telegram.TipBot) Lnurl {
	return Lnurl{
		c:                bot.Client,
		database:         bot.DB.Users,
		callbackHostname: internal.Configuration.Bot.LNURLHostUrl,
		WebhookServer:    internal.Configuration.Lnbits.WebhookServer,
		buntdb:           bot.Bunt,
		telegram:         bot.Telegram,
		cache:            bot.Cache,
	}
}
func (lnurlInvoice Invoice) Key() string {
	return fmt.Sprintf("lnurl-p:%s", lnurlInvoice.PaymentHash)
}

func (w Lnurl) Handle(writer http.ResponseWriter, request *http.Request) {
	var err error
	var response interface{}

	username := mux.Vars(request)["username"]
	if username == "" || len(username) > 64 {
		response = lnurlError("Invalid user.")
		err = fmt.Errorf("invalid username")
	} else if request.URL.RawQuery == "" {
		response, err = w.serveLNURLpFirst(username)
	} else if !lnurlIPLimiter.allow(clientIP(request)) || !lnurlUserLimiter.allow(strings.ToLower(username)) {
		response = lnurlError("Rate limit exceeded. Try later.")
		err = fmt.Errorf("rate limited")
	} else {
		stringAmount := request.FormValue("amount")
		if stringAmount == "" {
			response = lnurlError("Amount is not set.")
			err = fmt.Errorf("amount is not set")
		} else {
			var amount int64
			if amount, err = strconv.ParseInt(stringAmount, 10, 64); err != nil {
				amount, err = telegram.GetAmount(stringAmount)
				if err != nil {
					response = lnurlError("Invalid amount.")
					err = fmt.Errorf("invalid amount")
				} else {
					amount *= 1000
				}
			}

			if err == nil {
				comment := sanitizeComment(request.FormValue("comment"))

				var payerData lnurl.PayerDataValues
				if payerdata := request.FormValue("payerdata"); payerdata != "" {
					if uerr := json.Unmarshal([]byte(payerdata), &payerData); uerr != nil {
						log.Errorf("[handleLnUrl] Couldn't parse payerdata: %v", uerr)
					}
				}

				response, err = w.serveLNURLpSecond(username, amount, comment, payerData)
			}
		}
	}

	if err != nil {
		log.Errorf("[LNURL] %v", err.Error())
		if response != nil {
			if werr := api.WriteResponse(writer, response); werr != nil {
				api.NotFoundHandler(writer, werr)
			}
			return
		}
		api.NotFoundHandler(writer, err)
		return
	}

	if werr := api.WriteResponse(writer, response); werr != nil {
		api.NotFoundHandler(writer, werr)
	}
}

func (w Lnurl) getMetaDataCached(username string) lnurl.Metadata {
	key := fmt.Sprintf("lnurl_metadata_%s", username)

	// load metadata from cache
	if m, err := w.cache.Get(key); err == nil {
		return m.(lnurl.Metadata)
	}

	// otherwise, create new metadata
	metadata := w.metaData(username)

	// load the user profile picture
	if internal.Configuration.Bot.LNURLSendImage {
		// get the user from the database
		user, tx := findUser(w.database, username)
		if tx.Error == nil && user.Telegram != nil {
			addImageToMetaData(w.telegram, &metadata, username, user.Telegram)
		}
	}

	// save into cache
	runtime.IgnoreError(w.cache.Set(key, metadata, &store.Options{Expiration: 30 * time.Minute}))
	return metadata
}

// serveLNURLpFirst serves the first part of the LNURLp protocol with the endpoint
// to call and the metadata that matches the description hash of the second response
func (w Lnurl) serveLNURLpFirst(username string) (interface{}, error) {
	user, tx := findUser(w.database, username)
	if tx.Error != nil || userCannotReceiveLNURL(user) {
		return lnurlError("Invalid user."), fmt.Errorf("invalid or banned user")
	}

	log.Infof("[LNURL] Serving endpoint for user %s", username)
	callbackURL, err := url.Parse(fmt.Sprintf("%s/%s/%s", w.callbackHostname.String(), Endpoint, username))
	if err != nil {
		return lnurlError("Invalid user."), err
	}

	// produce the metadata including the image
	metadata := w.getMetaDataCached(username)

	return &lnurl.LNURLPayParams{
		LNURLResponse:   lnurl.LNURLResponse{Status: api.StatusOk},
		Tag:             PayRequestTag,
		Callback:        callbackURL.String(),
		MinSendable:     MinSendable,
		MaxSendable:     maxSendableMsat(),
		EncodedMetadata: metadata.Encode(),
		CommentAllowed:  CommentAllowed,
		PayerData: &lnurl.PayerDataSpec{
			FreeName:         &lnurl.PayerDataItemSpec{},
			LightningAddress: &lnurl.PayerDataItemSpec{},
			Email:            &lnurl.PayerDataItemSpec{},
		},
	}, nil
}

// serveLNURLpSecond serves the second LNURL response with the payment request with the correct description hash
func (w Lnurl) serveLNURLpSecond(username string, amount_msat int64, comment string, payerData lnurl.PayerDataValues) (*lnurl.LNURLPayValues, error) {
	log.Infof("[LNURL] Serving invoice for user %s", username)

	maxMsat := maxSendableMsat()
	if amount_msat < MinSendable || amount_msat > maxMsat {
		return &lnurl.LNURLPayValues{
			LNURLResponse: lnurl.LNURLResponse{
				Status: api.StatusError,
				Reason: fmt.Sprintf("Amount out of bounds (min: %d sat, max: %d sat).",
					MinSendable/1000, maxMsat/1000),
			},
		}, fmt.Errorf("amount out of bounds")
	}

	// check comment length
	comment = sanitizeComment(comment)
	if len(comment) > CommentAllowed {
		return &lnurl.LNURLPayValues{
			LNURLResponse: lnurl.LNURLResponse{
				Status: api.StatusError,
				Reason: fmt.Sprintf("Comment too long (max: %d characters).", CommentAllowed),
			},
		}, fmt.Errorf("comment too long")
	}

	user, tx := findUser(w.database, username)
	if tx.Error != nil || userCannotReceiveLNURL(user) {
		log.Infof("[LNURL] reject invoice mint for %s (missing/banned)", username)
		return invalidUser()
	}

	// user is ok now create invoice
	// set wallet lnbits client

	// the same description_hash needs to be built in the second request
	metadata := w.getMetaDataCached(username)

	var payerDataByte []byte
	if payerData.Email != "" || payerData.LightningAddress != "" || payerData.FreeName != "" {
		b, err := json.Marshal(payerData)
		if err != nil {
			return lnurlError("Couldn't create invoice."), fmt.Errorf("[serveLNURLpSecond] payerdata: %w", err)
		}
		payerDataByte = b
	}

	descriptionHash, err := w.DescriptionHash(metadata, string(payerDataByte))
	if err != nil {
		return lnurlError("Couldn't create invoice."), fmt.Errorf("[serveLNURLpSecond] description hash: %w", err)
	}

	invoice, err := user.Wallet.Invoice(
		lnbits.InvoiceParams{
			Amount:          amount_msat / 1000,
			Out:             false,
			DescriptionHash: descriptionHash,
			Webhook:         w.WebhookServer,
		},
		w.c,
	)
	if err != nil {
		return lnurlError("Couldn't create invoice."), fmt.Errorf("[serveLNURLpSecond] Couldn't create invoice: %w", err)
	}
	invoiceStruct := &telegram.Invoice{
		PaymentRequest: invoice.PaymentRequest,
		PaymentHash:    invoice.PaymentHash,
		Amount:         amount_msat / 1000,
	}
	// save lnurl invoice struct for later use (will hold the comment or other metadata for a notification when paid)
	runtime.IgnoreError(w.buntdb.Set(
		Invoice{
			Invoice:   invoiceStruct,
			User:      user,
			Comment:   comment,
			CreatedAt: time.Now(),
			From:      extractSenderFromPayerdata(payerData),
		}))
	// save the invoice Event that will be loaded when the invoice is paid and trigger the comment display callback
	runtime.IgnoreError(w.buntdb.Set(
		telegram.InvoiceEvent{
			Invoice:  invoiceStruct,
			User:     user,
			Callback: telegram.InvoiceCallbackLNURLPayReceive,
		}))

	return &lnurl.LNURLPayValues{
		LNURLResponse: lnurl.LNURLResponse{Status: api.StatusOk},
		PR:            invoice.PaymentRequest,
		Routes:        make([]struct{}, 0),
		SuccessAction: &lnurl.SuccessAction{Message: "Payment received!", Tag: "message"},
	}, nil

}

// DescriptionHash is the SHA256 hash of the metadata
func (w Lnurl) DescriptionHash(metadata lnurl.Metadata, payerData string) (string, error) {
	var hashString string
	var hash [32]byte
	if len(payerData) == 0 {
		hash = sha256.Sum256([]byte(metadata.Encode()))
		hashString = hex.EncodeToString(hash[:])
	} else {
		hash = sha256.Sum256([]byte(metadata.Encode() + payerData))
		hashString = hex.EncodeToString(hash[:])
	}
	return hashString, nil
}

// metaData returns the metadata that is sent in the first response
// and is used again in the second response to verify the description hash
func (w Lnurl) metaData(username string) lnurl.Metadata {
	// this is a bit stupid but if the address is a UUID starting with 1x...
	// we actually want to find the users username so it looks nicer in the
	// metadata description
	if strings.HasPrefix(username, "1x") {
		user, _ := findUser(w.database, username)
		if user.Telegram.Username != "" {
			username = user.Telegram.Username
		}
	}

	return lnurl.Metadata{
		Description:      fmt.Sprintf("Pay to %s@%s", username, w.callbackHostname.Hostname()),
		LightningAddress: fmt.Sprintf("%s@%s", username, w.callbackHostname.Hostname()),
	}
}

// addImageMetaData add images an image to the LNURL metadata
func addImageToMetaData(tb *tb.Bot, metadata *lnurl.Metadata, username string, user *tb.User) {
	metadata.Image.Ext = "jpeg"

	// if the username is anonymous, add the bot's picture
	if isAnonUsername(username) {
		metadata.Image.Bytes = telegram.BotProfilePicture
		return
	}

	// if the user has a profile picture, add it
	picture, err := telegram.DownloadProfilePicture(tb, user)
	if err != nil {
		log.Debugf("[LNURL] Couldn't download user %s's profile picture: %v", username, err)
		// in case the user has no image, use bot's picture
		metadata.Image.Bytes = telegram.BotProfilePicture
		return
	}
	metadata.Image.Bytes = picture
}

func isAnonUsername(username string) bool {
	if _, err := strconv.ParseInt(username, 10, 64); err == nil {
		return true
	} else {
		return strings.HasPrefix(username, "0x")
	}
}

func findUser(database *gorm.DB, username string) (*lnbits.User, *gorm.DB) {
	// now check for the user
	user := &lnbits.User{}
	// check if "username" is actually the user ID
	tx := database
	if _, err := strconv.ParseInt(username, 10, 64); err == nil {
		// asume it's anon_id
		tx = database.Where("anon_id = ?", username).First(user)
	} else if strings.HasPrefix(username, "0x") {
		// asume it's anon_id_sha256
		tx = database.Where("anon_id_sha256 = ?", username).First(user)
	} else if strings.HasPrefix(username, "1x") {
		// asume it's uuid
		tx = database.Where("uuid = ?", username).First(user)
	} else {
		// assume it's a string @username
		tx = database.Where("telegram_username = ? COLLATE NOCASE", username).First(user)
	}
	return user, tx
}

func extractSenderFromPayerdata(payer lnurl.PayerDataValues) string {
	if payer.LightningAddress != "" {
		return payer.LightningAddress
	}
	if payer.Email != "" {
		return payer.Email
	}
	if payer.FreeName != "" {
		return payer.FreeName
	}
	return ""
}
