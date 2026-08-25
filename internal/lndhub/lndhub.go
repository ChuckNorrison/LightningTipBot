package lndhub

import (
	"net/http"

	"github.com/ChuckNorrison/LightningTipBot/internal"
	"github.com/ChuckNorrison/LightningTipBot/internal/api"
	"github.com/ChuckNorrison/LightningTipBot/internal/telegram"
	"gorm.io/gorm"
)

type LndHub struct {
	database *gorm.DB
}

func New(bot *telegram.TipBot) LndHub {
	return LndHub{database: bot.DB.Users}
}
func (w LndHub) Handle(writer http.ResponseWriter, request *http.Request) {
	api.Proxy(writer, request, internal.Configuration.Lnbits.Url)
}
