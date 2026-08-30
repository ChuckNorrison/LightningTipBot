package telegram

import (
	"context"
	stderrors "errors"
	"fmt"
	"strconv"
	"time"
	"strings"

	"github.com/ChuckNorrison/LightningTipBot/internal/telegram/intercept"

	"github.com/ChuckNorrison/LightningTipBot/internal/errors"

	log "github.com/sirupsen/logrus"

	"github.com/ChuckNorrison/LightningTipBot/internal/lnbits"
	"github.com/ChuckNorrison/LightningTipBot/internal/str"
	tb "gopkg.in/lightningtipbot/telebot.v3"
	"gorm.io/gorm"
)

func (bot TipBot) repairWalletLink(user *lnbits.User) error {
    if user == nil {
        log.Errorln("[repairWalletLink] user is nil")
        return nil
    }
    if user.Name == "" {
        log.Errorln("[repairWalletLink] user.Name empty, skip")
        return nil
    }

    log.Errorf("[repairWalletLink] START name=%s id=%s wallet_id=%v",
        user.Name, user.ID, user.Wallet != nil)

    foundUser, foundWallet, err := bot.Client.FindUserAndWalletByTelegramID(user.Name)
    if err != nil {
        log.Errorln("[repairWalletLink] search failed:", err.Error())
        return err
    }
    if foundWallet == nil {
        log.Errorln("[repairWalletLink] no matching wallet found")
        return nil
    }

    log.Errorf("[repairWalletLink] found user=%s wallet=%s name=%q balance=%d local_wallet=%v",
        foundUser.ID, foundWallet.ID, foundWallet.Name, foundWallet.BalanceSats(),
        user.Wallet != nil && user.Wallet.ID == foundWallet.ID)

    if foundWallet.Inkey == "" {
        log.Errorln("[repairWalletLink] found wallet has no inkey – abort")
        return fmt.Errorf("wallet without inkey")
    }
    log.Errorf("[repairWalletLink] linking wallet=%s inkey_len=%d sats=%d",
        foundWallet.ID, len(foundWallet.Inkey), foundWallet.BalanceSats())

    user.ID = foundUser.ID
    user.Wallet = foundWallet
    if user.AnonID == "" {
        user.AnonID = fmt.Sprint(str.Int32Hash(user.ID))
    }
    user.AnonIDSha256 = str.AnonIdSha256(user)
    user.UUID = str.UUIDSha256(user)

    err = UpdateUserRecord(user, bot)
    if err != nil {
        log.Errorln("[repairWalletLink] UpdateUserRecord failed:", err.Error())
        return err
    }
    log.Errorln("[repairWalletLink] DB updated OK")
    return nil
}

func (bot TipBot) startHandler(ctx intercept.Context) (intercept.Context, error) {
	if !ctx.Message().Private() {
		return ctx, errors.Create(errors.NoPrivateChatError)
	}
	// ATTENTION: DO NOT CALL ANY HANDLER BEFORE THE WALLET IS CREATED
	// WILL RESULT IN AN ENDLESS LOOP OTHERWISE
	// bot.helpHandler(m)
	log.Printf("[⭐️ /start] New user: %s (%d)\n", GetUserStr(ctx.Sender()), ctx.Sender().ID)
	walletCreationMsg := bot.trySendMessageEditable(ctx.Sender(), Translate(ctx, "startSettingWalletMessage"))
	user, err := bot.initWallet(ctx.Sender())
	if err != nil {
		log.Errorln(fmt.Sprintf("[startHandler] Error with initWallet: %s", err.Error()))
		bot.tryEditMessage(walletCreationMsg, Translate(ctx, "startWalletErrorMessage"))
		return ctx, err
	}
	bot.tryDeleteMessage(walletCreationMsg)
	ctx.Context = context.WithValue(ctx, "user", user)
	bot.helpHandler(ctx)
	bot.trySendMessage(ctx.Sender(), Translate(ctx, "startWalletReadyMessage"))
	bot.balanceHandler(ctx)

	// send the user a warning about the fact that they need to set a username
	if len(ctx.Sender().Username) == 0 {
		bot.trySendMessage(ctx.Sender(), Translate(ctx, "startNoUsernameMessage"), tb.NoPreview)
	}
	return ctx, nil
}

func (bot TipBot) createWallet(user *lnbits.User) error {
    UserStr := GetUserStr(user.Telegram)
    username := strconv.FormatInt(user.Telegram.ID, 10)
    walletName := fmt.Sprintf("%d (%s)", user.Telegram.ID, UserStr)

    u, err := bot.Client.CreateUserWithInitialWallet(
        username,
        walletName,
        "",
    )

    if err != nil && strings.Contains(strings.ToLower(err.Error()), "already exists") {
        log.Warnf("[createWallet] Username %s already exists, reusing...", username)
        foundUser, foundWallet, ferr := bot.Client.FindUserAndWalletByTelegramID(username)
        if ferr != nil {
            log.Errorln("[createWallet] Reuse failed:", ferr.Error())
            return ferr
        }
        user.ID = foundUser.ID
        user.Name = username
        user.Wallet = foundWallet
    } else if err != nil {
        log.Errorln("[createWallet] Create wallet error:", err.Error())
        return err
    } else {
        // create wallet success
        user.ID = u.ID
        user.Name = username

        var wallets []lnbits.Wallet
        if len(u.Wallets) > 0 {
            wallets = u.Wallets
        } else {
            wallets, err = bot.Client.Wallets(*user)
            if err != nil {
                log.Errorln("[createWallet] Get wallet error:", err.Error())
                return err
            }
        }
        if len(wallets) == 0 {
            return fmt.Errorf("[createWallet] no wallet for user %s", user.ID)
        }
        user.Wallet = lnbits.SelectBestWallet(wallets, username)
    }

    if user.Wallet == nil || user.Wallet.ID == "" {
        return fmt.Errorf("[createWallet] wallet empty for user %s", user.ID)
    }

    user.AnonID = fmt.Sprint(str.Int32Hash(user.ID))
    user.AnonIDSha256 = str.AnonIdSha256(user)
    user.UUID = str.UUIDSha256(user)
    user.Initialized = false
    if user.CreatedAt.IsZero() {
        user.CreatedAt = time.Now()
    }

    return UpdateUserRecord(user, bot)
}

// needsWalletRepair detects LNbits 1.x migration mismatches:
// local DB points at default empty "LNbits wallet" instead of "{telegramId} (@user)".
func needsWalletRepair(user *lnbits.User) bool {
    if user == nil {
        return false
    }
    if user.ID == "" || user.Wallet == nil || user.Wallet.ID == "" {
        return true
    }
    if user.Wallet.Inkey == "" && user.Wallet.Adminkey == "" {
        return true
    }

    tgID := user.Name
    if tgID == "" && user.Telegram != nil {
        tgID = strconv.FormatInt(user.Telegram.ID, 10)
    }

    name := strings.TrimSpace(user.Wallet.Name)
    if name == "" || strings.EqualFold(name, "LNbits wallet") {
        return true
    }
    // Healthy TipBot wallets are named like "123456789 (@username)"
    if tgID != "" && !strings.Contains(name, tgID) {
        return true
    }
    return false
}

func (bot TipBot) initWallet(tguser *tb.User) (*lnbits.User, error) {
    user, err := GetUser(tguser, bot)
    if stderrors.Is(err, gorm.ErrRecordNotFound) {
        user = &lnbits.User{Telegram: tguser}
        err = bot.createWallet(user)
        if err != nil {
            return user, err
        }
        user, err = GetUser(tguser, bot)
        if err != nil {
            return user, err
        }
        user.Initialized = true
        if err = UpdateUserRecord(user, bot); err != nil {
            return user, err
        }
    } else if user != nil && !user.Initialized {
        tipTooltipInitializedHandler(user.Telegram, bot)
        user.Initialized = true
        if err = UpdateUserRecord(user, bot); err != nil {
            return user, err
        }
    } else if user != nil && user.Initialized {
        // bereits initialisiert
    } else {
        return user, fmt.Errorf("could not initialize wallet")
    }

    // Compatibility: Try to repair/update wallet
    if user != nil && needsWalletRepair(user) {
        wname := ""
        if user.Wallet != nil {
            wname = user.Wallet.Name
        }
        log.Warnf("[initWallet] wallet mismatch for %s (name=%q) – repairWalletLink", user.Name, wname)
        if rerr := bot.repairWalletLink(user); rerr != nil {
            log.Warnln("[initWallet] repairWalletLink:", rerr.Error())
        }
    }
    return user, nil
}
