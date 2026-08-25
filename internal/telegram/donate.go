package telegram

import (
	"context"
	"fmt"
	"strings"

	"github.com/ChuckNorrison/LightningTipBot/internal"

	"github.com/ChuckNorrison/LightningTipBot/internal/telegram/intercept"

	"github.com/ChuckNorrison/LightningTipBot/internal/errors"

	"github.com/ChuckNorrison/LightningTipBot/internal/str"

	"github.com/ChuckNorrison/LightningTipBot/internal/lnbits"

	log "github.com/sirupsen/logrus"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

// AS THE PROJECT WAS ABANDONED AND FORKED BY ChuckNorrison
// DONATIONS WILL BE RECEIVED BY THE RUNNING BOT OWNER FROM NOW
// THANKS TO ALL CONTRIBUTORS OF THIS PROJECT

var (
	donationEndpoint string
)

func helpDonateUsage(ctx context.Context, errormsg string) string {
	if len(errormsg) > 0 {
		return fmt.Sprintf(Translate(ctx, "donateHelpText"), fmt.Sprintf("%s", errormsg))
	} else {
		return fmt.Sprintf(Translate(ctx, "donateHelpText"), "")
	}
}

func (bot TipBot) donationHandler(ctx intercept.Context) (intercept.Context, error) {
    m := ctx.Message()
    bot.anyTextHandler(ctx)

    user := LoadUser(ctx)
    if user.Wallet == nil {
        return ctx, errors.Create(errors.UserNoWalletError)
    }

    amount, err := decodeAmountFromCommand(m.Text)
    if (err != nil || amount < 1) && m.Chat.Type == tb.ChatPrivate {
        _, err = bot.askForAmount(ctx, "", "CreateDonationState", 0, 0, m.Text)
        return ctx, err
    }
    if err != nil || amount < 1 {
        bot.trySendMessage(m.Chat, helpDonateUsage(ctx, Translate(ctx, "donateInvalidAmountMessage")))
        return ctx, err
    }

    msg := bot.trySendMessageEditable(m.Chat, Translate(ctx, "donationProgressMessage"))

    // Recipient: this bot's own wallet
    botUser, err := GetUser(bot.Telegram.Me, bot)
    if err != nil || botUser == nil || botUser.Wallet == nil {
        log.Errorln("[/donate] bot wallet not initialized:", err)
        bot.tryEditMessage(msg, Translate(ctx, "donationErrorMessage"))
        return ctx, fmt.Errorf("bot wallet not ready")
    }

    // Ensure bot wallet link is healthy (optional but useful after LNbits upgrades)
    if rerr := bot.repairWalletLink(botUser); rerr != nil {
        log.Warnln("[/donate] repairWalletLink:", rerr.Error())
    }
    botUser, err = GetUser(bot.Telegram.Me, bot)
    if err != nil || botUser.Wallet == nil {
        bot.tryEditMessage(msg, Translate(ctx, "donationErrorMessage"))
        return ctx, fmt.Errorf("bot wallet not ready after repair")
    }

    memo := fmt.Sprintf("Donation from %s", GetUserStr(user.Telegram))
    inv, err := botUser.Wallet.Invoice(
        lnbits.InvoiceParams{
            Out:     false,
            Amount:  amount, // same unit as working /invoice; use amount*1000 if your API expects msat
            Memo:    memo,
            Webhook: internal.Configuration.Lnbits.WebhookServer,
        },
        bot.Client,
    )
    if err != nil {
        log.Errorln("[/donate] create invoice:", err)
        bot.tryEditMessage(msg, Translate(ctx, "donationErrorMessage"))
        return ctx, err
    }
    if inv.PaymentRequest == "" && inv.Bolt11 != "" {
        inv.PaymentRequest = inv.Bolt11
    }
    if inv.PaymentRequest == "" {
        log.Errorln("[/donate] empty bolt11 from LNbits")
        bot.tryEditMessage(msg, Translate(ctx, "donationErrorMessage"))
        return ctx, fmt.Errorf("empty bolt11")
    }

    _, err = user.Wallet.Pay(
        lnbits.PaymentParams{Out: true, Bolt11: inv.PaymentRequest},
        bot.Client,
    )
    if err != nil {
        log.Errorf("[/donate] Donation failed for user %s: %s", GetUserStr(user.Telegram), err)
        bot.tryEditMessage(msg, Translate(ctx, "donationErrorMessage"))
        return ctx, err
    }

    bot.tryDeleteMessage(msg)
    bot.trySendMessage(m.Chat, Translate(ctx, "donationSuccess"))
    return ctx, nil
}

func (bot TipBot) parseCmdDonHandler(ctx intercept.Context) error {
    m := ctx.Message()
    arg := ""

    if strings.HasPrefix(strings.ToLower(m.Text), "/send") {
        arg, _ = getArgumentFromCommand(m.Text, 2)
        if arg != "@"+bot.Telegram.Me.Username {
            return fmt.Errorf("err")
        }
    }

    if strings.HasPrefix(strings.ToLower(m.Text), "/tip") {
        if m.ReplyTo == nil || m.ReplyTo.Sender == nil {
            return fmt.Errorf("err")
        }
        arg = GetUserStr(m.ReplyTo.Sender)
        if arg != "@"+bot.Telegram.Me.Username {
            return fmt.Errorf("err")
        }
    }

    // Only intercept payments addressed to this bot instance
    if len(arg) < 1 || arg != "@"+bot.Telegram.Me.Username {
        return fmt.Errorf("err")
    }

    amount, err := decodeAmountFromCommand(m.Text)
    if err != nil {
        return err
    }

    donationInterceptMessage := fmt.Sprintf(
        "Thank you! I'm routing this donation to @%s.",
        bot.Telegram.Me.Username,
    )

    bot.trySendMessage(m.Sender, str.MarkdownEscape(donationInterceptMessage))
    m.Text = fmt.Sprintf("/donate %d", amount)
    bot.donationHandler(ctx)
    // nil = abort parent handler (/send or /tip)
    return nil
}
