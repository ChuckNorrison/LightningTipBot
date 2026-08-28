package webhook

import (
	"encoding/json"
	"fmt"
	"time"
	"io"
	"sync"

	"github.com/ChuckNorrison/LightningTipBot/internal"
	"github.com/ChuckNorrison/LightningTipBot/internal/lnbits"
	"github.com/ChuckNorrison/LightningTipBot/internal/telegram"

	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"

	"net/http"

	"github.com/ChuckNorrison/LightningTipBot/internal/storage"

	"github.com/gorilla/mux"
	tb "gopkg.in/lightningtipbot/telebot.v3"

	"github.com/ChuckNorrison/LightningTipBot/internal/i18n"
)

type Server struct {
	httpServer *http.Server
	bot        *tb.Bot
	c          *lnbits.Client
	database   *gorm.DB
	buntdb     *storage.DB
}

type Webhook struct {
	CheckingID    string               `json:"checking_id"`
	Pending       bool                 `json:"pending"`
	Amount        int64                `json:"amount"`
	Fee           int64                `json:"fee"`
	Memo          string               `json:"memo"`
	Time          json.RawMessage      `json:"time"`
	Bolt11        string               `json:"bolt11"`
	Preimage      string               `json:"preimage"`
	PaymentHash   string               `json:"payment_hash"`
	Extra         json.RawMessage      `json:"extra"`
	WalletID      string               `json:"wallet_id"`
	Webhook       string               `json:"webhook"`
	WebhookStatus interface{}          `json:"webhook_status"`
}

var (
    seenMu sync.Mutex
    seen   = map[string]time.Time{}
)

func alreadySeen(hash string) bool {
    if hash == "" {
        return false
    }
    seenMu.Lock()
    defer seenMu.Unlock()
    if _, ok := seen[hash]; ok {
        return true
    }
    seen[hash] = time.Now()
    return false
}

func NewServer(bot *telegram.TipBot) *Server {
	srv := &http.Server{
		Addr:         internal.Configuration.Lnbits.WebhookServerUrl.Host,
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}
	apiServer := &Server{
		c:          bot.Client,
		database:   bot.DB.Users,
		bot:        bot.Telegram,
		httpServer: srv,
		buntdb:     bot.Bunt,
	}
	apiServer.httpServer.Handler = apiServer.newRouter()
	go apiServer.httpServer.ListenAndServe()
	log.Infof("[Webhook] Server started at %s", internal.Configuration.Lnbits.WebhookServerUrl)
	return apiServer
}

func (w *Server) GetUserByWalletId(walletId string) (*lnbits.User, error) {
	user := &lnbits.User{}
	tx := w.database.Where("wallet_id = ?", walletId).First(user)
	if tx.Error != nil {
		return user, tx.Error
	}
	return user, nil
}

func (w *Server) newRouter() *mux.Router {
	router := mux.NewRouter()
	router.HandleFunc("/", w.receive).Methods(http.MethodPost)
	return router
}

func (w *Server) receive(writer http.ResponseWriter, request *http.Request) {
    log.Debugln("[Webhook] Received request")

    body, err := io.ReadAll(request.Body)
    if err != nil {
        log.Errorf("[Webhook] Error reading body: %s", err.Error())
        writer.WriteHeader(400)
        return
    }

    if len(body) > 0 && body[0] == '"' {
        var s string
        if err := json.Unmarshal(body, &s); err != nil {
            log.Errorf("[Webhook] Error unwrapping string: %s", err.Error())
            writer.WriteHeader(400)
            return
        }
        body = []byte(s)
    }

    webhookEvent := Webhook{}
    err = json.Unmarshal(body, &webhookEvent)
    if err != nil {
        log.Errorf("[Webhook] Error decoding request: %s body=%s", err.Error(), string(body))
        writer.WriteHeader(400)
        return
    }

    if alreadySeen(webhookEvent.PaymentHash) {
        log.Infof("[Webhook] duplicate %s ignored", webhookEvent.PaymentHash)
        writer.WriteHeader(200)
        return
    }

    user, err := w.GetUserByWalletId(webhookEvent.WalletID)
    if err != nil {
        log.Errorf("[Webhook] Error getting user: %s", err.Error())
        writer.WriteHeader(400)
        return
    }
    log.Infoln(fmt.Sprintf("[⚡️ WebHook] User %s (%d) received invoice of %d sat.", telegram.GetUserStr(user.Telegram), user.Telegram.ID, webhookEvent.Amount/1000))

    writer.WriteHeader(200)

    txInvoiceEvent := &telegram.InvoiceEvent{Invoice: &telegram.Invoice{PaymentHash: webhookEvent.PaymentHash}}
    err = w.buntdb.Get(txInvoiceEvent)
    if err != nil {
        log.Errorln(err)
    } else {
        if c := telegram.InvoiceCallback[txInvoiceEvent.Callback]; c.Function != nil {
            if err := telegram.AssertEventType(txInvoiceEvent, c.Type); err != nil {
                log.Errorln(err)
                return
            }
            go c.Function(txInvoiceEvent)
            return
        }
    }

    _, err = w.bot.Send(user.Telegram, fmt.Sprintf(i18n.Translate(user.Telegram.LanguageCode, "invoiceReceivedMessage"), webhookEvent.Amount/1000))
    if err != nil {
        log.Errorln(err)
    }
}
