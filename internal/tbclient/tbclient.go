// Package tbclient wraps the TigerBeetle Go client with Canvas Clash domain
// conventions. All claim construction lives here so the game server and the
// load generator build byte-identical transfers.
package tbclient

import (
	"encoding/binary"
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

// PixelIDMarker occupies the high 64 bits of every pixel account id, putting
// pixel accounts in a dedicated 128-bit namespace. This guarantees pixel ids
// are never zero (TB rejects zero) and never collide with SystemPoolID (1).
const PixelIDMarker uint64 = 1

// fundMarker occupies the high 64 bits of the deterministic fund-transfer id,
// keeping it distinct from UUIDv7 claim ids and from pixel account ids.
const fundMarker uint64 = 0xF00D

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

// PixelID packs (x, y) into a stable TigerBeetle account id. The pixel's
// coordinates live in the low 64 bits; the high 64 bits are PixelIDMarker.
func PixelID(x, y uint32) tb.Uint128 {
	var b [16]byte
	binary.LittleEndian.PutUint64(b[0:8], uint64(x)<<32|uint64(y))
	binary.LittleEndian.PutUint64(b[8:16], PixelIDMarker)
	return tb.BytesToUint128(b)
}

// UnpackPixelID recovers (x, y) from a PixelID. ok is false if the id's high
// bits aren't PixelIDMarker (i.e., it is not a pixel account id).
func UnpackPixelID(id tb.Uint128) (x, y uint32, ok bool) {
	lo, hi := id.Uint64()
	if hi != PixelIDMarker {
		return 0, 0, false
	}
	return uint32(lo >> 32), uint32(lo & 0xffffffff), true
}

// EnsureAccounts idempotently creates the system pool plus any pixel accounts.
// AccountExists and AccountCreated are both treated as success.
func (c *Client) EnsureAccounts(pixelIDs ...tb.Uint128) error {
	return c.createAccounts(true, pixelIDs...)
}

// createAccounts creates the given pixel accounts (and optionally the system
// pool). Batched callers pass includeSystemPool=false so the pool isn't
// re-added to every batch.
func (c *Client) createAccounts(includeSystemPool bool, pixelIDs ...tb.Uint128) error {
	accounts := make([]tb.Account, 0, len(pixelIDs)+1)
	if includeSystemPool {
		accounts = append(accounts, tb.Account{
			ID:     SystemPoolID,
			Ledger: LedgerCanvas,
			Code:   AccountCodeSystem,
		})
	}
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

// batchSize stays just under TigerBeetle's 8190 max so a full batch always fits.
const batchSize = 8189

// EnsureAllPixels eagerly creates and funds every pixel account in the grid in
// batches. One million pixels ≈ 122 create batches + 122 fund batches.
// Idempotent across restarts (AccountExists / TransferExists are success).
func (c *Client) EnsureAllPixels(w, h uint32) error {
	if err := c.EnsureAccounts(); err != nil { // system pool once
		return err
	}

	n := int(w) * int(h)
	ids := make([]tb.Uint128, 0, batchSize)
	for i := 0; i < n; i++ {
		ids = append(ids, PixelID(uint32(i)%w, uint32(i)/w))
		if len(ids) == batchSize {
			if err := c.createAccounts(false, ids...); err != nil {
				return err
			}
			ids = ids[:0]
		}
	}
	if len(ids) > 0 {
		if err := c.createAccounts(false, ids...); err != nil {
			return err
		}
	}

	transfers := make([]tb.Transfer, 0, batchSize)
	for i := 0; i < n; i++ {
		x, y := uint32(i)%w, uint32(i)/w
		transfers = append(transfers, tb.Transfer{
			ID:              FundID(x, y),
			DebitAccountID:  SystemPoolID,
			CreditAccountID: PixelID(x, y),
			Amount:          tb.ToUint128(1),
			Code:            TransferCodeRefund,
			Ledger:          LedgerCanvas,
		})
		if len(transfers) == batchSize {
			if err := c.submitFundBatch(transfers); err != nil {
				return err
			}
			transfers = transfers[:0]
		}
	}
	if len(transfers) > 0 {
		if err := c.submitFundBatch(transfers); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) submitFundBatch(transfers []tb.Transfer) error {
	results, err := c.tb.CreateTransfers(transfers)
	if err != nil {
		return fmt.Errorf("tbclient: fund batch: %w", err)
	}
	for _, r := range results {
		if r.Status != tb.TransferCreated && r.Status != tb.TransferExists {
			return fmt.Errorf("tbclient: fund batch failed: status=%s", r.Status)
		}
	}
	return nil
}

// FundID derives the deterministic fund-transfer id for a pixel. Using a
// stable id makes funding idempotent across server restarts (exists == ok).
func FundID(x, y uint32) tb.Uint128 {
	var b [16]byte
	binary.LittleEndian.PutUint64(b[0:8], uint64(x)<<32|uint64(y))
	binary.LittleEndian.PutUint64(b[8:16], fundMarker)
	return tb.BytesToUint128(b)
}

// fundTransfer builds the single-phase transfer that credits a pixel with its
// one spendable unit. The deterministic FundID makes re-submission idempotent.
func fundTransfer(x, y uint32) tb.Transfer {
	return tb.Transfer{
		ID:              FundID(x, y),
		DebitAccountID:  SystemPoolID,
		CreditAccountID: PixelID(x, y),
		Amount:          tb.ToUint128(1),
		Code:            TransferCodeRefund,
		Ledger:          LedgerCanvas,
	}
}

// Fund credits the pixel account with its single spendable unit. Idempotent:
// re-submitting returns TransferExists and is treated as success.
func (c *Client) Fund(x, y uint32) error {
	return c.SubmitBatch([]tb.Transfer{fundTransfer(x, y)})
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

// QueryCanvasTransfers returns canvas-ledger transfers with a timestamp at or
// after fromTimestamp, ascending. Used for boot-time cache warm-up.
func (c *Client) QueryCanvasTransfers(fromTimestamp uint64, limit uint32) ([]tb.Transfer, error) {
	transfers, err := c.tb.QueryTransfers(tb.QueryFilter{
		Ledger:       LedgerCanvas,
		TimestampMin: fromTimestamp,
		Limit:        limit,
	})
	if err != nil {
		return nil, fmt.Errorf("tbclient: query_transfers: %w", err)
	}
	return transfers, nil
}
