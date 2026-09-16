package notify

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/sideshow/apns2"
	"github.com/sideshow/apns2/payload"
	"github.com/sideshow/apns2/token"

	"github.com/onegator/gator/internal/server/store/db"
)

// APNs delivers through Apple. It is built only when the key is configured.
type APNs struct {
	client *apns2.Client
	topic  string
}

// LoadAPNs reads GATOR_APNS_KEY (path to the .p8), GATOR_APNS_KEY_ID, GATOR_APNS_TEAM_ID and
// GATOR_APNS_TOPIC (the app's bundle id). GATOR_APNS_SANDBOX=1 uses Apple's test servers.
// Without a key it returns nil, and the caller keeps the disabled sender.
func LoadAPNs() (*APNs, error) {
	path, keyID, teamID, topic := os.Getenv("GATOR_APNS_KEY"), os.Getenv("GATOR_APNS_KEY_ID"),
		os.Getenv("GATOR_APNS_TEAM_ID"), os.Getenv("GATOR_APNS_TOPIC")
	if path == "" && keyID == "" && teamID == "" {
		return nil, nil
	}
	if path == "" || keyID == "" || teamID == "" || topic == "" {
		return nil, errors.New("apns: GATOR_APNS_KEY, _KEY_ID, _TEAM_ID and _TOPIC must be set together")
	}
	key, err := token.AuthKeyFromFile(path)
	if err != nil {
		return nil, fmt.Errorf("apns key: %w", err)
	}
	client := apns2.NewTokenClient(&token.Token{AuthKey: key, KeyID: keyID, TeamID: teamID})
	if os.Getenv("GATOR_APNS_SANDBOX") == "1" {
		client = client.Development()
	} else {
		client = client.Production()
	}
	return &APNs{client: client, topic: topic}, nil
}

// Name implements Sender.
func (a *APNs) Name() string { return "apns " + a.topic }

// Send implements Sender. A token Apple rejects is reported as gone so it stops being used.
func (a *APNs) Send(ctx context.Context, device db.Device, m Message) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body := payload.NewPayload().AlertTitle(m.Title).AlertBody(m.Body).Sound("default").Custom("link", m.Link)
	res, err := a.client.PushWithContext(ctx, &apns2.Notification{
		DeviceToken: device.Token,
		Topic:       a.topic,
		Payload:     body,
		Expiration:  time.Now().Add(6 * time.Hour),
	})
	if err != nil {
		return err
	}
	switch res.Reason {
	case "":
		return nil
	case apns2.ReasonUnregistered, apns2.ReasonBadDeviceToken, apns2.ReasonDeviceTokenNotForTopic:
		return fmt.Errorf("%w: %s", ErrDeviceGone, res.Reason)
	}
	return fmt.Errorf("apns %d: %s", res.StatusCode, res.Reason)
}
