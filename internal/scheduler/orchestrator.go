package scheduler

import (
	"context"

	"github.com/gotd/td/telegram/message"

	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/bridge"
	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/config"
	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/storage"
	"github.com/Maxim-Vasyutin/telegram-proxy-userbot/internal/tg"
)

// ClientController adapts *tg.Client to the AccountController interface.
// Connect also wires up the Bridge for the account so relay starts
// immediately after connection is established.
type ClientController struct {
	client *tg.Client
	store  storage.Storage
	pairs  []config.PairConfig
}

// NewClientController returns a ClientController for the given tg.Client.
func NewClientController(client *tg.Client, store storage.Storage, pairs []config.PairConfig) *ClientController {
	return &ClientController{
		client: client,
		store:  store,
		pairs:  pairs,
	}
}

// Connect brings the account online and wires up the Bridge for relay.
// Implements AccountController.
func (c *ClientController) Connect(ctx context.Context) error {
	if err := c.client.Connect(ctx); err != nil {
		return err
	}

	sender := message.NewSender(c.client.API())
	br := bridge.New(
		c.store,
		sender,
		c.client.API(),
		c.pairs,
		c.client.SelfID(),
		c.client.Phone(),
	)
	if err := br.ResolvePeers(ctx, c.client.API()); err != nil {
		// Disconnect cleanly so the caller can retry.
		_ = c.client.Disconnect(ctx)
		return err
	}
	c.client.SetBridge(br)
	return nil
}

// Disconnect tears the account down.
// Implements AccountController.
func (c *ClientController) Disconnect(ctx context.Context) error {
	return c.client.Disconnect(ctx)
}

// Phone returns the account identifier.
// Implements AccountController.
func (c *ClientController) Phone() string {
	return c.client.Phone()
}
