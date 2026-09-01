package replay

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"

	tb "github.com/tigerbeetle/tigerbeetle-go"

	"pixelbeetle/internal/tbclient"
)

// cdcU128 decodes a TigerBeetle u128 from its decimal JSON string — the one
// encoding the pinned `tigerbeetle amqp` CDC producer emits. A future
// encoder change should surface here as a loud parse error (which the
// consumer acks as a poison pill) rather than as silent tolerance of a
// second format.
type cdcU128 struct {
	V tb.Uint128
}

func (f *cdcU128) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		f.V = tb.ToUint128(0)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("replay: u128 must be a decimal JSON string, got %s", b)
	}
	bi, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return fmt.Errorf("replay: invalid u128 decimal string %q", s)
	}
	f.V = tb.BigIntToUint128(bi)
	return nil
}

// cdcU64 decodes a u64 from its decimal JSON string (see cdcU128).
type cdcU64 uint64

func (f *cdcU64) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*f = 0
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("replay: u64 must be a decimal JSON string, got %s", b)
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return fmt.Errorf("replay: invalid u64 decimal string %q: %w", s, err)
	}
	*f = cdcU64(v)
	return nil
}

type cdcTransfer struct {
	ID          cdcU128 `json:"id"`
	Amount      cdcU128 `json:"amount"`
	PendingID   cdcU128 `json:"pending_id"`
	UserData128 cdcU128 `json:"user_data_128"`
	Code        uint16  `json:"code"`
	Flags       uint16  `json:"flags"`
	Timestamp   cdcU64  `json:"timestamp"`
}

type cdcAccount struct {
	ID cdcU128 `json:"id"`
}

type cdcBody struct {
	Timestamp     cdcU64      `json:"timestamp"`
	Type          string      `json:"type"`
	Ledger        uint32      `json:"ledger"`
	Transfer      cdcTransfer `json:"transfer"`
	DebitAccount  cdcAccount  `json:"debit_account"`
	CreditAccount cdcAccount  `json:"credit_account"`
}

// ParseMessage decodes a CDC message body into an Event. It derives the pixel
// coordinates and color for claim events (pending/posted debit the pixel and
// carry the color in the transfer code). Any malformed body — including a
// missing event type — is an error; the consumer acks it as a poison pill.
func ParseMessage(body []byte) (Event, error) {
	var m cdcBody
	if err := json.Unmarshal(body, &m); err != nil {
		return Event{}, fmt.Errorf("replay: parse cdc body: %w", err)
	}

	ev := Event{
		Type:       EventType(m.Type),
		TransferID: m.Transfer.ID.V,
		Player:     tbclient.Uint128ToUUID(m.Transfer.UserData128.V),
	}
	if ev.Type == "" {
		return Event{}, fmt.Errorf("replay: missing event type")
	}
	if m.Transfer.Timestamp != 0 {
		ev.Timestamp = uint64(m.Transfer.Timestamp)
	} else {
		ev.Timestamp = uint64(m.Timestamp)
	}

	// Claims debit the pixel account, so the pixel id is on the debit side.
	switch ev.Type {
	case TypePosted, TypePending:
		if x, y, ok := tbclient.UnpackPixelID(m.DebitAccount.ID.V); ok {
			ev.X, ev.Y = x, y
			if m.Transfer.Code >= tbclient.TransferCodeClaim {
				ev.Color = uint8(m.Transfer.Code - tbclient.TransferCodeClaim)
			}
		}
	}
	return ev, nil
}
