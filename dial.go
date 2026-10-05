package aznet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"net"
	"time"
)

// Dial is analogous to net.Dial. It takes a network type (e.g. "azblob")
// and an address (e.g. "https://account.blob.core.windows.net/?handshake=...").
func Dial(network, address string, opts ...Option) (conn net.Conn, err error) {
	driver, _, cfg, err := initialize(network, address, opts)
	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			cfg.cancel()
		}
	}()
	dialCtx, dialCancel := context.WithTimeout(cfg.ctx, cfg.connectTimeout)
	defer dialCancel()

	connID := uuid.New().String()
	noise, err := NewNoiseClient()
	if err != nil {
		return nil, err
	}
	msg1, err := noise.WriteMessage([]byte(connID))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoiseMsgFailed, err)
	}

	if err := driver.PostHandshake(dialCtx, connID, msg1); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrHandshakeExchangeFailed, err)
	}

	var encryptedTokens []byte
	for {
		data, err := driver.GetToken(dialCtx, connID)
		if err == nil {
			encryptedTokens = data
			break
		}
		if !errors.Is(err, ErrNoData) {
			return nil, err
		}

		select {
		case <-dialCtx.Done():
			return nil, dialCtx.Err()
		case <-time.After(cfg.dataPoll):
		}
	}

	payload, err := noise.ReadMessage(encryptedTokens)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}

	var tokens SessionTokens
	if err := json.Unmarshal(payload, &tokens); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecodeTokenFailed, err)
	}

	if !noise.IsComplete() {
		return nil, ErrHandshakeIncomplete
	}

	transport, err := driver.NewTransport(dialCtx, connID, tokens, true)
	if err == nil {
		err = dialCtx.Err()
	}
	if err != nil {
		if transport != nil {
			err = errors.Join(err, disposeTransport(transport))
		}
		return nil, err
	}

	c := newConn(cfg.ctx, cfg.cancel, transport, cfg, noise, driver, connID)
	c.sessionExpiry = tokens.ExpiresAt
	return c, nil
}
