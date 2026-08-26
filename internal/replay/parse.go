package replay

import (
	"encoding/json"
	"fmt"
	"math/big"

	tb "github.com/tigerbeetle/tigerbeetle-go"

	"pixelbeetle/internal/tbclient"
)

// flexU128 accepts a TigerBeetle u128 encoded as either a decimal JSON string
// (the documented form) or a bare JSON number (seen in doc examples). We
// support both so consumers are robust to the CDC encoder's choice.
type flexU128 struct {
	V tb.Uint128
}

func (f *flexU128) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		f.V = tb.ToUint128(0)
		return nil
	}
	var bi *big.Int
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		var ok bool
		bi, ok = new(big.Int).SetString(s, 10)
		if !ok {
			return fmt.Errorf("invalid u128 decimal string %q", s)
		}
	} else {
		// Bare JSON numbers may exceed uint64; json.Number preserves them
		// exactly as text so big.Int can parse arbitrary precision.
		var n json.Number
		if err := json.Unmarshal(b, &n); err != nil {
			return fmt.Errorf("invalid u128 number %s: %w", string(b), err)
		}
		var ok bool
		bi, ok = new(big.Int).SetString(n.String(), 10)
		if !ok {
			return fmt.Errorf("invalid u128 number %s", string(b))
		}
	}
	f.V = tb.BigIntToUint128(bi)
	return nil
}

// flexU64 accepts a u64 encoded as either a decimal JSON string or a bare
// JSON number.
type flexU64 uint64

func (f *flexU64) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*f = 0
		return nil
	}
	var bi *big.Int
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		var ok bool
		bi, ok = new(big.Int).SetString(s, 10)
		if !ok {
			return fmt.Errorf("invalid u64 decimal string %q", s)
		}
	} else {
		var n json.Number
		if err := json.Unmarshal(b, &n); err != nil {
			return fmt.Errorf("invalid u64 number %s: %w", string(b), err)
		}
		var ok bool
		bi, ok = new(big.Int).SetString(n.String(), 10)
		if !ok {
			return fmt.Errorf("invalid u64 number %s", string(b))
		}
	}
	if !bi.IsUint64() {
		return fmt.Errorf("u64 value overflows uint64")
	}
	*f = flexU64(bi.Uint64())
	return nil
}

type cdcTransfer struct {
	ID          flexU128 `json:"id"`
	Amount      flexU128 `json:"amount"`
	PendingID   flexU128 `json:"pending_id"`
	UserData128 flexU128 `json:"user_data_128"`
	Code        uint16   `json:"code"`
	Flags       uint16   `json:"flags"`
	Timestamp   flexU64  `json:"timestamp"`
}

type cdcAccount struct {
	ID flexU128 `json:"id"`
}

type cdcBody struct {
	Timestamp     flexU64     `json:"timestamp"`
	Type          string      `json:"type"`
	Ledger        uint32      `json:"ledger"`
	Transfer      cdcTransfer `json:"transfer"`
	DebitAccount  cdcAccount  `json:"debit_account"`
	CreditAccount cdcAccount  `json:"credit_account"`
}

// ParseMessage decodes a CDC message body into an Event. It derives the pixel
// coordinates and color for claim events (pending/posted debit the pixel and
// carry the color in the transfer code).
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
		ev.Type = TypeSingle
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
