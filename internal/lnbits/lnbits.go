package lnbits

import (
    "crypto/rand"
    "encoding/hex"
    "fmt"
    "strings"
    "sync"
    "time"

    "github.com/imroc/req"
    log "github.com/sirupsen/logrus"
)

func (c *Client) FindUserAndWalletByTelegramID(telegramID string) (User, *Wallet, error) {
    var empty User
    if err := c.ensureToken(); err != nil {
        return empty, nil, err
    }

    resp, err := req.Get(c.url+"/users/api/v1/user", c.header)
    if err != nil {
        return empty, nil, err
    }
    if resp.Response().StatusCode >= 300 {
        var reqErr Error
        resp.ToJSON(&reqErr)
        return empty, nil, reqErr
    }

    var wrapped struct {
        Data []User `json:"data"`
    }
    if err := resp.ToJSON(&wrapped); err != nil {
        return empty, nil, err
    }

    var bestUser User
    var bestWallet *Wallet

    for _, u := range wrapped.Data {
        full, err := c.GetUser(u.ID)
        if err != nil {
            continue
        }
        for i := range full.Wallets {
            w := full.Wallets[i]
            if w.Name == "" || !strings.Contains(w.Name, telegramID) {
                // telegram user wallet name should be like "12345678 (@username)", skip
                continue
            }

            if w.Inkey == "" && w.Adminkey == "" {
                log.Warnf("[FindUserAndWallet] skip %s – no keys", w.ID)
                continue
            }
            bal := w.BalanceSats()
            log.Errorf("[FindUserAndWallet] candidate id=%s name=%q sats=%d msat=%d inkey=%d",
                w.ID, w.Name, bal, w.BalanceMsat, len(w.Inkey))

            // remember wallet with highest balance
            if bestWallet == nil || bal > bestWallet.BalanceSats() {
                bestUser = full
                cp := w
                bestWallet = &cp
            }
        }
    }

    if bestWallet == nil {
        return empty, nil, fmt.Errorf("no wallet containing %s with keys", telegramID)
    }
    // after the best wallet was found, validate again GetUser, Keys sicher übernehmen
    full, err := c.GetUser(bestUser.ID)
    if err == nil {
        for i := range full.Wallets {
            if full.Wallets[i].ID == bestWallet.ID {
                cp := full.Wallets[i]
                bestWallet = &cp
                bestUser = full
                break
            }
        }
    }
    return bestUser, bestWallet, nil
}

func (c *Client) FindUserByUsername(username string) (user User, err error) {
    if err = c.ensureToken(); err != nil {
        return
    }

    resp, err := req.Get(c.url+"/users/api/v1/user", c.header)
    if err != nil {
        return
    }
    if resp.Response().StatusCode >= 300 {
        var reqErr Error
        resp.ToJSON(&reqErr)
        err = reqErr
        return
    }

    var wrapped struct {
        Data []User `json:"data"`
    }
    if err = resp.ToJSON(&wrapped); err != nil {
        return
    }

    for _, u := range wrapped.Data {
        if u.ID == username || u.Name == username || u.Username == username {
            return c.GetUser(u.ID)
        }

        full, gerr := c.GetUser(u.ID)
        if gerr != nil {
            continue
        }

        if full.Name == username || full.Username == username {
            return full, nil
        }
        if strings.Contains(full.Name, username) || strings.Contains(full.Username, username) {
            return full, nil
        }

        for _, w := range full.Wallets {
            if w.Name == username || strings.HasPrefix(w.Name, username+" ") || strings.Contains(w.Name, username) {
                full.Wallet = &w
                return full, nil
            }
        }
    }

    err = fmt.Errorf("user with telegram-id %s not found in LNbits", username)
    return
}

func SelectBestWallet(wallets []Wallet, telegramID string) *Wallet {
    if len(wallets) == 0 {
        return nil
    }

    log.Warnf("[SelectBestWallet] %d wallet(s) for telegramID=%s", len(wallets), telegramID)
    for i := range wallets {
        log.Warnf("[SelectBestWallet]   [%d] id=%s name=%q balance=%d",
            i, wallets[i].ID, wallets[i].Name, wallets[i].Balance)
    }

    for i := range wallets {
        if telegramID != "" && strings.Contains(wallets[i].Name, telegramID) {
            log.Warnf("[SelectBestWallet] chosen by name match: %s", wallets[i].ID)
            return &wallets[i]
        }
    }

    best := &wallets[0]
    for i := range wallets {
        if wallets[i].BalanceSats() > best.BalanceSats() {
            best = &wallets[i]
        }
    }
    return best
}

func generateRandomPassword() string {
    b := make([]byte, 16)
    _, err := rand.Read(b)
    if err != nil {
        return fmt.Sprintf("pw%d", time.Now().UnixNano())
    }
    return hex.EncodeToString(b)
}

type Client struct {
    url            string
    header         req.Header
    adminKey       string
    adminUsername  string
    adminPassword  string
    accessToken    string
    tokenExpiresAt time.Time
    mu             sync.Mutex
}

func NewClient(url string) *Client {
    return &Client{
        url:      url,
        header: req.Header{
            "Content-Type": "application/json",
            "Accept":       "application/json",
        },
    }
}

func (c *Client) SetAdminCredentials(username, password string) {
    c.adminUsername = username
    c.adminPassword = password
}

func (c *Client) ensureToken() error {
    c.mu.Lock()
    defer c.mu.Unlock()

    // Reuse the cached Bearer token if it is still valid.
    // Refresh 5 minutes before actual expiry to avoid edge-of-expiry failures.
    if c.accessToken != "" && time.Now().Before(c.tokenExpiresAt.Add(-5*time.Minute)) {
        c.header = req.Header{
            "Content-Type":  "application/json",
            "Accept":        "application/json",
            "Authorization": "Bearer " + c.accessToken,
        }
        return nil
    }

    if c.adminUsername == "" || c.adminPassword == "" {
        return fmt.Errorf("admin_username / admin_password not set in config")
    }

    body := map[string]string{
        "username": c.adminUsername,
        "password": c.adminPassword,
    }

    // Send authentication
    resp, err := req.Post(c.url+"/api/v1/auth", req.BodyJSON(body))
    if err != nil {
        return err
    }
    if resp.Response().StatusCode >= 300 {
        var reqErr Error
        resp.ToJSON(&reqErr)
        return reqErr
    }

    var loginResp struct {
        AccessToken string `json:"access_token"`
    }
    if err := resp.ToJSON(&loginResp); err != nil {
        return err
    }

    c.accessToken = loginResp.AccessToken
    c.tokenExpiresAt = time.Now().Add(30 * 24 * time.Hour)

    c.header = req.Header{
        "Content-Type":  "application/json",
        "Accept":        "application/json",
        "Authorization": "Bearer " + c.accessToken,
    }
    return nil
}

func (c *Client) GetUser(userId string) (user User, err error) {
    if err = c.ensureToken(); err != nil {
        return
    }
    resp, err := req.Get(c.url+"/users/api/v1/user/"+userId, c.header)
    if err != nil {
        return
    }
    if resp.Response().StatusCode >= 300 {
        var reqErr Error
        resp.ToJSON(&reqErr)
        err = reqErr
        return
    }
    err = resp.ToJSON(&user)
    return
}

func (c *Client) CreateUserWithInitialWallet(userName, walletName, email string) (wal User, err error) {
    if err = c.ensureToken(); err != nil {
        return
    }

    password := generateRandomPassword()
    body := map[string]interface{}{
        "username":        userName,
        "password":        password,
        "password_repeat": password,
    }
    if walletName != "" {
        body["wallet_name"] = walletName
    }

    resp, err := req.Post(c.url+"/users/api/v1/user", c.header, req.BodyJSON(body))
    if err != nil {
        return
    }
    if resp.Response().StatusCode >= 300 {
        var reqErr Error
        resp.ToJSON(&reqErr)
        err = reqErr
        return
    }

    err = resp.ToJSON(&wal)
    if err != nil {
        return
    }

    if wal.ID != "" {
        fullUser, err2 := c.GetUser(wal.ID)
        if err2 == nil {
            wal = fullUser
            if len(wal.Wallets) > 0 {
                wal.Wallet = &wal.Wallets[0]
            }
        }
    }
    return
}

func (c *Client) CreateWallet(userId, walletName, adminId string) (wal Wallet, err error) {
    if err = c.ensureToken(); err != nil {
        return
    }
    body := map[string]string{"name": walletName}
    resp, err := req.Post(c.url+"/api/v1/wallet", c.header, req.BodyJSON(body))
    if err != nil {
        return
    }
    if resp.Response().StatusCode >= 300 {
        var reqErr Error
        resp.ToJSON(&reqErr)
        err = reqErr
        return
    }
    err = resp.ToJSON(&wal)
    return
}

func (w Wallet) Invoice(params InvoiceParams, c *Client) (lntx Invoice, err error) {
    invoiceHeader := req.Header{
        "Content-Type": "application/json",
        "Accept":       "application/json",
        "X-Api-Key":    w.Inkey,
    }
    resp, err := req.Post(c.url+"/api/v1/payments", invoiceHeader, req.BodyJSON(&params))
    if err != nil {
        return
    }
    // Format error in json and return
    if resp.Response().StatusCode >= 300 {
        var reqErr Error
        resp.ToJSON(&reqErr)
        err = reqErr
        return
    }
    err = resp.ToJSON(&lntx)
    if err != nil {
        return
    }
    if lntx.PaymentRequest == "" && lntx.Bolt11 != "" {
        lntx.PaymentRequest = lntx.Bolt11
    }
    return
}

func (c Client) Info(w Wallet) (wtx Wallet, err error) {
    invoiceHeader := req.Header{
        "Content-Type": "application/json",
        "Accept":       "application/json",
        "X-Api-Key":    w.Inkey,
    }
    resp, err := req.Get(c.url+"/api/v1/wallet", invoiceHeader)
    if err != nil {
        return
    }
    if resp.Response().StatusCode >= 300 {
        var reqErr Error
        resp.ToJSON(&reqErr)
        err = reqErr
        return
    }

    log.Errorf("[Info] status=%d body=%s", resp.Response().StatusCode, resp.String())

    err = resp.ToJSON(&wtx)
    return
}

func (c Client) Payments(w Wallet) (wtx Payments, err error) {
    invoiceHeader := req.Header{
        "Content-Type": "application/json",
        "Accept":       "application/json",
        "X-Api-Key":    w.Inkey,
    }
    resp, err := req.Get(c.url+"/api/v1/payments?limit=60", invoiceHeader)
    if err != nil {
        return
    }
    if resp.Response().StatusCode >= 300 {
        var reqErr Error
        resp.ToJSON(&reqErr)
        err = reqErr
        return
    }
    err = resp.ToJSON(&wtx)
    return
}

func (c Client) Payment(w Wallet, payment_hash string) (payment LNbitsPayment, err error) {
    invoiceHeader := req.Header{
        "Content-Type": "application/json",
        "Accept":       "application/json",
        "X-Api-Key":    w.Inkey,
    }
    resp, err := req.Get(c.url+fmt.Sprintf("/api/v1/payments/%s", payment_hash), invoiceHeader)
    if err != nil {
        return
    }
    if resp.Response().StatusCode >= 300 {
        var reqErr Error
        resp.ToJSON(&reqErr)
        err = reqErr
        return
    }
    err = resp.ToJSON(&payment)
    return
}

func (c Client) Wallets(w User) (wtx []Wallet, err error) {
    user, err := c.GetUser(w.ID)
    if err != nil {
        return
    }
    if user.Wallets != nil {
        wtx = user.Wallets
    }
    return
}

func (w Wallet) Pay(params PaymentParams, c *Client) (wtx Invoice, err error) {
    adminHeader := req.Header{
        "Content-Type": "application/json",
        "Accept":       "application/json",
        "X-Api-Key":    w.Adminkey,
    }
    r := req.New()
    r.SetTimeout(time.Hour * 24)
    resp, err := r.Post(c.url+"/api/v1/payments", adminHeader, req.BodyJSON(&params))
    if err != nil {
        return
    }
    if resp.Response().StatusCode >= 300 {
        var reqErr Error
        resp.ToJSON(&reqErr)
        err = reqErr
        return
    }
    err = resp.ToJSON(&wtx)
    return
}
