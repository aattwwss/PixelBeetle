package replay

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"

	tb "github.com/tigerbeetle/tigerbeetle-go"

	"pixelbeetle/internal/tbclient"
)

// cdcU128 decodes a TigerBeetle u128 from either encoding the
// `tigerbeetle amqp` CDC producer emits on the wire: a decimal JSON string
// (captured fixtures) or a bare JSON number (live-verified 2026-09-01 — the
// strings-only parser rejected real events; see feedback.md #15). Anything
// else is a loud parse error, which the consumer acks as a poison pill.
type cdcU128 struct {
	V tb.Uint128
}

func (f *cdcU128) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		f.V = tb.ToUint128(0)
		return nil
	}
	s := string(b)
	if b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("replay: invalid u128 string %s", b)
		}
	}
	bi, ok := new(big.Int).SetString(s, 10)
	if !ok || bi.Sign() < 0 {
		return fmt.Errorf("replay: invalid u128 decimal %q", s)
	}
	if bi.BitLen() > 128 {
		return fmt.Errorf("replay: u128 out of range %q", s)
	}
	f.V = tb.BigIntToUint128(bi)
	return nil
}

// cdcU64 decodes a u64 from a decimal string or a bare JSON number (see
// cdcU128). ParseUint's range check covers overflow in both forms.
type cdcU64 uint64

func (f *cdcU64) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*f = 0
		return nil
	}
	s := string(b)
	if b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("replay: invalid u64 string %s", b)
		}
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return fmt.Errorf("replay: invalid u64 decimal %q: %w", s, err)
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
