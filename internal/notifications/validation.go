package notifications

import (
	"fmt"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

// SetPreferenceRequest is the PUT /v1/notifications/preferences body: the caller's
// full enabled-channel set for one notification type. tenant_id/app_user_id are
// NOT here — they come from the verified principal. An empty Channels slice is a
// valid, explicit full opt-out for the type (distinct from omitting the type
// entirely, which leaves the default — every channel enabled — in force).
type SetPreferenceRequest struct {
	Type     string   `json:"type"`
	Channels []string `json:"channels"`
}

// Validate enforces the boundary rules via ozzo (method-based, not struct tags):
// type is required, and every entry in channels must be a known delivery channel
// (ValidChannel) — an unrecognized value is a 400, not a silently-ignored no-op
// preference. A failure here is a 400 at the edge (KindInvalid → 400).
func (r SetPreferenceRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Type, validation.Required),
		validation.Field(&r.Channels, validation.Each(validation.By(validDeliveryChannel))),
	)
}

// validDeliveryChannel is the ozzo rule: the value must be a known delivery
// channel (the same set notification_delivery.channel accepts).
func validDeliveryChannel(value any) error {
	c, _ := value.(string)
	if !ValidChannel(c) {
		return fmt.Errorf("must be one of: %s, %s", ChannelEmail, ChannelInApp)
	}
	return nil
}
