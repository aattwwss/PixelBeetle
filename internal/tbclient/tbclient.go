// Package tbclient wraps the TigerBeetle Go client with Canvas Clash domain
// conventions. All claim construction lives here so the game server and the
// load generator build byte-identical transfers.
package tbclient

import (
	"fmt"

	"github.com/google/uuid"
	tb "github.com/tigerbeetle/tigerbeetle-go"
)

// Domain conventions (see plan.md §1).
const (
	LedgerCanvas uint32 = 1 // ledger 1 == "Canvas"

	AccountCodeSystem uint16 = 999
	AccountCodePixel  uint16 = 1000

	// Transfer code carries the chosen color (0–255).
	TransferCodeClaim uint16 = 1000 // base code; color OR'd in by NewClaim

	ClaimTimeoutSeconds uint32 = 3 // pending-lock window
)

// SystemPoolID is the fixed dummy debit-side account.
var SystemPoolID = tb.ToUint128(1)

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
		})
	}
	results, err := c.tb.CreateAccounts(accounts)
	if err != nil {
		return fmt.Errorf("tbclient: create_accounts: %w", err)
	}
	for _, r := range results {
		if r.Status != tb.AccountCreated && r.Status != tb.AccountExists {
			// NOTE: verify exact index encoding against a live cluster;
			// Reserved likely carries the failing batch index.
			return fmt.Errorf("tbclient: create_accounts failed: status=%d reserved=%d", r.Status, r.Reserved)
		}
	}
	return nil
}

// NewClaim builds the pending transfer that locks a pixel for a player.
//
//	id            = fresh UUIDv7 (idempotent retries, LSM-friendly ordering)
//	debit         = system pool (keeps double-entry balanced)
//	credit        = pixel account (balance becomes the pixel version counter)
//	code          = the color
//	user_data_128 = the player id
func NewClaim(x, y uint32, color uint8, player uuid.UUID) tb.Transfer {
	flags := tb.TransferFlags{Pending: true}
	return tb.Transfer{
		ID:              UUIDToUint128(uuid.Must(uuid.NewV7())),
		DebitAccountID:  SystemPoolID,
		CreditAccountID: PixelID(x, y),
		Amount:          tb.ToUint128(1),
		UserData128:     tb.BytesToUint128(player),
		Code:            TransferCodeClaim | uint16(color),
		Timeout:         ClaimTimeoutSeconds,
		Ledger:          LedgerCanvas,
		Flags:           flags.ToUint16(),
	}
}

// BuildPost finalizes a pending claim (two-phase commit: commit leg).
// The commit/rollback leg references the pending transfer via PendingID and
// must repeat the pending transfer's code exactly.
func BuildPost(claimID tb.Uint128, color uint8) tb.Transfer {
	flags := tb.TransferFlags{PostPendingTransfer: true}
	return tb.Transfer{
		ID:        UUIDToUint128(uuid.Must(uuid.NewV7())),
		PendingID: claimID,
		Code:      TransferCodeClaim | uint16(color),
		Flags:     flags.ToUint16(),
	}
}

// BuildVoid discards a pending claim early (two-phase commit: rollback leg).
func BuildVoid(claimID tb.Uint128, color uint8) tb.Transfer {
	flags := tb.TransferFlags{VoidPendingTransfer: true}
	return tb.Transfer{
		ID:        UUIDToUint128(uuid.Must(uuid.NewV7())),
		PendingID: claimID,
		Code:      TransferCodeClaim | uint16(color),
		Flags:     flags.ToUint16(),
	}
}

func (c *Client) Submit(t tb.Transfer) error {
	results, err := c.tb.CreateTransfers([]tb.Transfer{t})
	if err != nil {
		return fmt.Errorf("tbclient: create_transfers: %w", err)
	}
	for _, r := range results {
		if r.Status != tb.TransferCreated && r.Status != tb.TransferPendingTransferAlreadyPosted {
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
