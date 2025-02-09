package telegram

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"

	"github.com/LightningTipBot/LightningTipBot/internal/errors"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"

	"github.com/LightningTipBot/LightningTipBot/internal"

	log "github.com/sirupsen/logrus"

	"github.com/LightningTipBot/LightningTipBot/internal/i18n"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/runtime"
	"github.com/LightningTipBot/LightningTipBot/internal/str"
	"github.com/skip2/go-qrcode"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

type InvoiceEventCallback map[int]EventHandler

type EventHandler struct {
	Function func(event Event)
	Type     EventType
}

var InvoiceCallback InvoiceEventCallback

func initInvoiceEventCallbacks(bot *TipBot) {
	InvoiceCallback = InvoiceEventCallback{
		InvoiceCallbackGeneric:         EventHandler{Function: bot.notifyInvoiceReceivedEvent, Type: EventTypeInvoice},
		InvoiceCallbackInlineReceive:   EventHandler{Function: bot.inlineReceiveEvent, Type: EventTypeInvoice},
		InvoiceCallbackLNURLPayReceive: EventHandler{Function: bot.lnurlReceiveEvent, Type: EventTypeInvoice},
		InvoiceCallbackGroupTicket:     EventHandler{Function: bot.groupGetInviteLinkHandler, Type: EventTypeInvoice},
		InvoiceCallbackSatdressProxy:   EventHandler{Function: bot.satdressProxyRelayPaymentHandler, Type: EventTypeInvoice},
		InvoiceCallbackGenerateDalle:   EventHandler{Function: bot.generateDalleImages, Type: EventTypeInvoice},
	}
}

type InvoiceEventKey int

const (
	InvoiceCallbackGeneric = iota + 1
	InvoiceCallbackInlineReceive
	InvoiceCallbackLNURLPayReceive
	InvoiceCallbackGroupTicket
	InvoiceCallbackSatdressProxy
	InvoiceCallbackGenerateDalle
)

const (
	EventTypeInvoice       EventType = "invoice"
	EventTypeTicketInvoice EventType = "ticket-invoice"
)

type EventType string

func AssertEventType(event Event, eventType EventType) error {
	if event.Type() != eventType {
		return fmt.Errorf("invalid event type")
	}
	return nil
}

type Invoice struct {
	PaymentHash    string `json:"payment_hash"`
	PaymentRequest string `json:"payment_request"`
	Amount         int64  `json:"amount"`
	Memo           string `json:"memo"`
}
type InvoiceEvent struct {
	*Invoice
	*storage.Base
	User           *lnbits.User `json:"user"`                      // the user that is being paid
	Message        *tb.Message  `json:"message,omitempty"`         // the message that the invoice replies to
	InvoiceMessage *tb.Message  `json:"invoice_message,omitempty"` // the message that displays the invoice
	LanguageCode   string       `json:"languagecode"`              // language code of the user
	Callback       int          `json:"func"`                      // which function to call if the invoice is paid
	CallbackData   string       `json:"callbackdata"`              // add some data for the callback
	Chat           *tb.Chat     `json:"chat,omitempty"`            // if invoice is supposed to be sent to a particular chat
	Payer          *lnbits.User `json:"payer,omitempty"`           // if a particular user is supposed to pay this
}

func (invoiceEvent InvoiceEvent) Type() EventType {
	return EventTypeInvoice
}

type Event interface {
	Type() EventType
}

func (invoiceEvent InvoiceEvent) Key() string {
	return fmt.Sprintf("invoice:%s", invoiceEvent.PaymentHash)
}

func helpInvoiceUsage(ctx context.Context, errormsg string) string {
	if len(errormsg) > 0 {
		return fmt.Sprintf(Translate(ctx, "invoiceHelpText"), fmt.Sprintf("%s", errormsg))
	} else {
		return fmt.Sprintf(Translate(ctx, "invoiceHelpText"), "")
	}
}

func (bot *TipBot) invoiceHandler(ctx intercept.Context) (intercept.Context, error) {
	m := ctx.Message()
	// check and print all commands
	bot.anyTextHandler(ctx)
	user := LoadUser(ctx)
	if user.Wallet == nil {
		return ctx, errors.Create(errors.UserNoWalletError)
	}
	userStr := GetUserStr(user.Telegram)
	if m.Chat.Type != tb.ChatPrivate {
		// delete message
		bot.tryDeleteMessage(m)
		return ctx, errors.Create(errors.NoPrivateChatError)
	}
	// if no amount is in the command, ask for it
	amount, err := decodeAmountFromCommand(m.Text)
	if (err != nil || amount < 1) && m.Chat.Type == tb.ChatPrivate {
		// // no amount was entered, set user state and ask fo""r amount
		_, err = bot.askForAmount(ctx, "", "CreateInvoiceState", 0, 0, m.Text)
		return ctx, err
	}

	// check for memo in command
	memo := "Powered by @LightningTipBot"
	if len(strings.Split(m.Text, " ")) > 2 {
		memo = GetMemoFromCommand(m.Text, 2)
		tag := " (@LightningTipBot)"
		memoMaxLen := 159 - len(tag)
		if len(memo) > memoMaxLen {
			memo = memo[:memoMaxLen-len(tag)]
		}
		memo = memo + tag
	}

	creatingMsg := bot.trySendMessageEditable(m.Sender, Translate(ctx, "lnurlGettingUserMessage"))
	log.Debugf("[/invoice] Creating invoice for %s of %d sat.", userStr, amount)
	invoice, err := bot.createInvoiceWithEvent(ctx, user, amount, memo, InvoiceCallbackGeneric, "")
	if err != nil {
		errmsg := fmt.Sprintf("[/invoice] Could not create an invoice: %s", err.Error())
		bot.tryEditMessage(creatingMsg, Translate(ctx, "errorTryLaterMessage"))
		log.Errorln(errmsg)
		return ctx, err
	}

	// create qr code
	qr, err := qrcode.Encode(invoice.PaymentRequest, qrcode.Medium, 256)
	if err != nil {
		errmsg := fmt.Sprintf("[/invoice] Failed to create QR code for invoice: %s", err.Error())
		bot.tryEditMessage(creatingMsg, Translate(ctx, "errorTryLaterMessage"))
		log.Errorln(errmsg)
		return ctx, err
	}

	// deleting messages will delete the main menu.
	//bot.tryDeleteMessage(creatingMsg)

	// send the invoice data to user
	bot.trySendMessage(m.Sender, &tb.Photo{File: tb.File{FileReader: bytes.NewReader(qr)}, Caption: fmt.Sprintf("`%s`", invoice.PaymentRequest)})
	log.Printf("[/invoice] Incvoice created. User: %s, amount: %d sat.", userStr, amount)
	return ctx, nil
}

func (bot *TipBot) createInvoiceWithEvent(ctx context.Context, user *lnbits.User, amount int64, memo string, callback int, callbackData string) (InvoiceEvent, error) {
	invoice, err := user.Wallet.Invoice(
		lnbits.InvoiceParams{
			Out:     false,
			Amount:  int64(amount),
			Memo:    memo,
			Webhook: internal.Configuration.Lnbits.WebhookServer},
		bot.Client)
	if err != nil {
		errmsg := fmt.Sprintf("[/invoice] Could not create an invoice: %s", err.Error())
		log.Errorln(errmsg)
		return InvoiceEvent{}, err
	}
	invoiceEvent := InvoiceEvent{
		Invoice: &Invoice{PaymentHash: invoice.PaymentHash,
			PaymentRequest: invoice.PaymentRequest,
			Amount:         amount,
			Memo:           memo},
		User:         user,
		Callback:     callback,
		CallbackData: callbackData,
		LanguageCode: ctx.Value("publicLanguageCode").(string),
	}
	// save invoice struct for later use
	runtime.IgnoreError(bot.Bunt.Set(invoiceEvent))
	return invoiceEvent, nil
}

func (bot *TipBot) notifyInvoiceReceivedEvent(event Event) {
	invoiceEvent := event.(*InvoiceEvent)
	// do balance check for keyboard update
	_, err := bot.GetUserBalance(invoiceEvent.User)
	if err != nil {
		errmsg := fmt.Sprintf("could not get balance of user %s", GetUserStr(invoiceEvent.User.Telegram))
		log.Errorln(errmsg)
	}

	bot.trySendMessage(invoiceEvent.User.Telegram, fmt.Sprintf(i18n.Translate(invoiceEvent.User.Telegram.LanguageCode, "invoiceReceivedMessage"), invoiceEvent.Amount))
}

type LNURLInvoice struct {
	*Invoice
	Comment   string       `json:"comment"`
	User      *lnbits.User `json:"user"`
	CreatedAt time.Time    `json:"created_at"`
	Paid      bool         `json:"paid"`
	PaidAt    time.Time    `json:"paid_at"`
	From      string       `json:"from"`
}

func (lnurlInvoice LNURLInvoice) Key() string {
	return fmt.Sprintf("lnurl-p:%s", lnurlInvoice.PaymentHash)
}

func (bot *TipBot) lnurlReceiveEvent(event Event) {
	invoiceEvent := event.(*InvoiceEvent)

	bot.notifyInvoiceReceivedEvent(invoiceEvent)
	tx := &LNURLInvoice{Invoice: &Invoice{PaymentHash: invoiceEvent.PaymentHash}}
	err := bot.Bunt.Get(tx)
	log.Debugf("[lnurl-p] Received invoice for %s of %d sat.", GetUserStr(invoiceEvent.User.Telegram), tx.Amount)
	if err == nil {
		if len(tx.Comment) > 0 {
			if len(tx.From) == 0 {
				bot.trySendMessage(tx.User.Telegram, fmt.Sprintf("✉️ %s", str.MarkdownEscape(tx.Comment)))
			} else {
				bot.trySendMessage(tx.User.Telegram, fmt.Sprintf("✉️ From `%s`: %s", tx.From, str.MarkdownEscape(tx.Comment)))
			}
		} else if len(tx.From) > 0 {
			bot.trySendMessage(tx.User.Telegram, fmt.Sprintf("From `%s`", str.MarkdownEscape(tx.From)))
		}
	}
}
