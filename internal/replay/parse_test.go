package replay

import (
	"fmt"
	"strings"
	"testing"

	"pixelbeetle/internal/tbclient"
)

func TestParseMessageStringU128s(t *testing.T) {
	pixelID := tbclient.PixelID(5, 6).BigInt().String()
	body := fmt.Sprintf(`{
		"timestamp":"1787682583188303701",
		"type":"two_phase_posted",
		"ledger":1,
		"transfer":{
			"id":"6376658857635782657",
			"amount":"1",
			"pending_id":"6376658857635782656",
			"user_data_128":"79248595801719937611592367840129079151",
			"code":1003,
			"flags":5,
			"timestamp":"1787682583188303701"
		},
		"debit_account":{
			"id":"%s",
			"code":1000,
			"flags":2,
			"timestamp":"1787682583188303701"
		},
		"credit_account":{
			"id":"1",
			"code":999,
			"flags":0,
			"timestamp":"1787682583188303701"
		}
	}`, pixelID)

	ev, err := ParseMessage([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Type != TypePosted {
		t.Fatalf("want type posted, got %q", ev.Type)
	}
	if ev.X != 5 || ev.Y != 6 || ev.Color != 3 {
		t.Fatalf("want (5,6,3), got (%d,%d,%d)", ev.X, ev.Y, ev.Color)
	}
	if ev.Timestamp != 1787682583188303701 {
		t.Fatalf("want ns timestamp, got %d", ev.Timestamp)
	}
}

func TestParseMessageNonClaimEvent(t *testing.T) {
	body := `{
		"timestamp":"1787682583188303703",
		"type":"two_phase_expired",
		"ledger":1,
		"transfer":{"id":"1","amount":"1","pending_id":"0","user_data_128":"0","code":1002,"flags":2,"timestamp":"1787682583188303703"},
		"debit_account":{"id":"1","code":1000,"flags":2,"timestamp":"1787682583188303703"},
		"credit_account":{"id":"1","code":999,"flags":0,"timestamp":"1787682583188303703"}
	}`
	ev, err := ParseMessage([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Type != TypeExpired {
		t.Fatalf("want type expired, got %q", ev.Type)
	}
	if ev.X != 0 || ev.Y != 0 || ev.Color != 0 {
		t.Fatalf("expired event should carry zero pixel coords, got (%d,%d,%d)", ev.X, ev.Y, ev.Color)
	}
}

// The producer emits ONE format (decimal strings). A bare JSON number is a
// loud parse error, not a silently-tolerated second format.
func TestParseMessageRejectsBareNumberU128s(t *testing.T) {
	pixelID := tbclient.PixelID(7, 8).BigInt().String()
	body := fmt.Sprintf(`{
		"timestamp":1787682583188303702,
		"type":"two_phase_posted",
		"ledger":1,
		"transfer":{"id":6376658857635782657,"amount":1,"pending_id":6376658857635782656,"user_data_128":"79248595801719937611592367840129079151","code":1009,"flags":5,"timestamp":1787682583188303702},
		"debit_account":{"id":%s,"code":1000,"flags":2,"timestamp":1787682583188303702},
		"credit_account":{"id":1,"code":999,"flags":0,"timestamp":1787682583188303702}
	}`, pixelID)
	if _, err := ParseMessage([]byte(body)); err == nil {
		t.Fatal("want parse error for bare-number u128s, got nil")
	}
}

func TestParseMessageRejectsOversizedTimestamp(t *testing.T) {
	big := strings.Repeat("9", 64) // > uint64 max
	body := fmt.Sprintf(`{
		"timestamp":"%s",
		"type":"two_phase_posted",
		"ledger":1,
		"transfer":{"id":"1","amount":"1","pending_id":"0","user_data_128":"0","code":1000,"flags":5,"timestamp":"%s"},
		"debit_account":{"id":"1","code":1000,"flags":2,"timestamp":"%s"},
		"credit_account":{"id":"1","code":999,"flags":0,"timestamp":"%s"}
	}`, big, big, big, big)
	if _, err := ParseMessage([]byte(body)); err == nil {
		t.Fatal("want parse error for overflowing timestamp, got nil")
	}
}

func TestParseMessageRejectsMissingType(t *testing.T) {
	body := `{
		"timestamp":"1787682583188303703",
		"ledger":1,
		"transfer":{"id":"1","amount":"1","pending_id":"0","user_data_128":"0","code":1000,"flags":5,"timestamp":"1787682583188303703"},
		"debit_account":{"id":"1","code":1000,"flags":2,"timestamp":"1787682583188303703"},
		"credit_account":{"id":"1","code":999,"flags":0,"timestamp":"1787682583188303703"}
	}`
	if _, err := ParseMessage([]byte(body)); err == nil {
		t.Fatal("want parse error for missing type, got nil")
	}
}
