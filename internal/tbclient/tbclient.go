// Package tbclient wraps the TigerBeetle Go client with Canvas Clash domain
// conventions. All claim construction lives here so the game server and the
// load generator build byte-identical transfers.
package tbclient

import (
	"fmt"

	"github.com/google/uuid"
	tb "github.com/tigerbeetle/tigerbeetle-go"
)

// Domain conventions (see plan.md §1 + docs/tigerbeetle-cheatsheet.md).
const (
	LedgerCanvas uint32 = 1 // ledger 1 == "Canvas"

	AccountCodeSystem uint16 = 999
	AccountCodePixel  uint16 = 1000

	// Transfer code carries the chosen color (0–255).
	TransferCodeClaim  uint16 = 1000 // base code; color OR'd in by NewClaim
	TransferCodeRefund uint16 = 1001 // re-fund leg after a posted claim

	ClaimTimeoutSeconds uint32 = 3 // pending-lock window
)

// SystemPoolID is the fixed dummy debit-side account.
var SystemPoolID = tb.ToUint128(1)

// ErrPixelLocked means TigerBeetle itself rejected the claim because the
// pixel's claimable unit is already reserved by a pending transfer.
var ErrPixelLocked = fmt.Errorf("pixel already claimed")

type Client struct {
	tb tb.Client
}

func Connect(clusterID uint64, addresses []string) (*Client, error) {
	c, err := tb.NewClient(tb.ToUint128(clusterID), addresses)
	if err != nil {
		return nil, fmt.Errorf("tbclient: connect: %w", err)
	}
	return &Client{tb: c}, nil
}

func (c *Client) Close() { c.tb.Close() }

// PixelID packs (x, y) into a stable TigerBeetle account id.
func PixelID(x, y uint32) tb.Uint128 {
	return tb.ToUint128(uint64(x)<<32 | uint64(y))
}

// EnsureAccounts idempotently creates the system pool plus any pixel accounts.
// AccountExists and AccountCreated are both treated as success.
func (c *Client) EnsureAccounts(pixelIDs ...tb.Uint128) error {
	accounts := make([]tb.Account, 0, len(pixelIDs)+1)
	accounts = append(accounts, tb.Account{
		ID:     SystemPoolID,
		Ledger: LedgerCanvas,
		Code:   AccountCodeSystem,
	})
	for _, id := range pixelIDs {
		accounts = append(accounts, tb.Account{
			ID:     id,
			Ledger: LedgerCanvas,
			Code:   AccountCodePixel,
			Flags: tb.AccountFlags{ // the exclusivity invariant lives HERE
				DebitsMustNotExceedCredits: true,
			}.ToUint16(),
		})
	}
	results, err := c.tb.CreateAccounts(accounts)
	if err != nil {
		return fmt.Errorf("tbclient: create_accounts: %w", err)
	}
	for _, r := range results {
		if r.Status != tb.AccountCreated && r.Status != tb.AccountExists {
			return fmt.Errorf("tbclient: create_accounts failed: status=%s", r.Status)
		}
	}
	return nil
}

// FundID derives the deterministic fund-transfer id for a pixel. Using a
// stable id makes funding idempotent across server restarts (exists == ok).
func FundID(x, y uint32) tb.Uint128 {
	var b [16]byte
	key := uint64(x)<<32 | uint64(y)
	b[0] = 0xF0 // marker byte: distinguishes fund ids from UUIDv7 claim ids
	for i := 0; i < 8; i++ {
		b[8+i] = byte(key >> (8 * (7 - i)))
	}
	return tb.BytesToUint128(b)
}

// Fund credits the pixel account with its single spendable unit. Idempotent:
// re-submitting returns TransferExists and is treated as success.
func (c *Client) Fund(x, y uint32) error {
	t := tb.Transfer{
		ID:              FundID(x, y),
		DebitAccountID:  SystemPoolID,
		CreditAccountID: PixelID(x, y),
		Amount:          tb.ToUint128(1),
		Code:            TransferCodeRefund,
		Ledger:          LedgerCanvas,
	}
	results, err := c.tb.CreateTransfers([]tb.Transfer{t})
	if err != nil {
		return fmt.Errorf("tbclient: fund: %w", err)
	}
	for _, r := range results {
		if r.Status != tb.TransferCreated && r.Status != tb.TransferExists {
			return fmt.Errorf("tbclient: fund failed: status=%s", r.Status)
		}
	}
	return nil
}

// NewClaim builds the pending transfer that locks a pixel for a player.
//
//	id            = fresh UUIDv7 (idempotent retries, LSM-friendly ordering)
//	debit         = PIXEL account — debits_must_not_exceed_credits makes a
//	                concurrent second claim fail AT CREATION with exceeds_credits
//	credit        = system pool
//	code          = the color
//	user_data_128 = the player id
func NewClaim(x, y uint32, color uint8, player uuid.UUID) tb.Transfer {
	flags := tb.TransferFlags{Pending: true}
	return tb.Transfer{
		ID:              UUIDToUint128(uuid.Must(uuid.NewV7())),
		DebitAccountID:  PixelID(x, y),
		CreditAccountID: SystemPoolID,
		Amount:          tb.ToUint128(1),
		UserData128:     tb.BytesToUint128(player),
		Code:            TransferCodeClaim | uint16(color),
		Timeout:         ClaimTimeoutSeconds,
		Ledger:          LedgerCanvas,
		Flags:           flags.ToUint16(),
	}
}

// BuildPost builds the post leg of a confirm. Resolution legs may omit
// debit/credit/ledger/code entirely (zero) — TB copies them from the pending.
func BuildPost(claimID tb.Uint128) tb.Transfer {
	flags := tb.TransferFlags{PostPendingTransfer: true}
	return tb.Transfer{
		ID:        UUIDToUint128(uuid.Must(uuid.NewV7())),
		PendingID: claimID,
		Flags:     flags.ToUint16(),
	}
}

// BuildVoid discards a pending claim early (two-phase commit: rollback leg).
func BuildVoid(claimID tb.Uint128) tb.Transfer {
	flags := tb.TransferFlags{VoidPendingTransfer: true}
	return tb.Transfer{
		ID:        UUIDToUint128(uuid.Must(uuid.NewV7())),
		PendingID: claimID,
		Flags:     flags.ToUint16(),
	}
}

// BuildConfirm is the atomic commit path: post the pending AND re-fund the
// pixel with its claimable unit in one linked batch. If either fails, both
// roll back — the pixel can never lose or double-gain a unit.
func BuildConfirm(claimID tb.Uint128, x, y uint32) []tb.Transfer {
	post := BuildPost(claimID)
	post.Flags = tb.TransferFlags{PostPendingTransfer: true, Linked: true}.ToUint16()
	refund := tb.Transfer{ // plain posted transfer: put the unit back
		ID:              UUIDToUint128(uuid.Must(uuid.NewV7())),
		DebitAccountID:  SystemPoolID,
		CreditAccountID: PixelID(x, y),
		Amount:          tb.ToUint128(1),
		Code:            TransferCodeRefund,
		Ledger:          LedgerCanvas,
	}
	return []tb.Transfer{post, refund}
}

// Submit sends one transfer; used by direct-mode bots and simple paths.
// Accepts created / exists / already-resolved as success.
func (c *Client) Submit(t tb.Transfer) error {
	return c.SubmitBatch([]tb.Transfer{t})
}

// SubmitBatch sends multiple transfers atomically as one request and maps
// result statuses to domain errors.
func (c *Client) SubmitBatch(transfers []tb.Transfer) error {
	results, err := c.tb.CreateTransfers(transfers)
	if err != nil {
		return fmt.Errorf("tbclient: create_transfers: %w", err)
	}
	for _, r := range results {
		switch r.Status {
		case tb.TransferCreated, tb.TransferExists,
			tb.TransferPendingTransferAlreadyPosted, tb.TransferPendingTransferAlreadyVoided:
			continue // idempotent successes
		case tb.TransferExceedsCredits:
			return ErrPixelLocked
		default:
			return fmt.Errorf("tbclient: create_transfers failed: status=%s", r.Status)
		}
	}
	return nil
}

func UUIDToUint128(id uuid.UUID) tb.Uint128 {
	return tb.BytesToUint128(id)
}

func Uint128ToUUID(v tb.Uint128) uuid.UUID {
	b := v.Bytes()
	u, _ := uuid.FromBytes(b[:])
	return u
}
