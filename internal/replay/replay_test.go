package replay

import (
	"fmt"
	"testing"

	"pixelbeetle/internal/tbclient"
)

type recordingSink struct{ evts []Event }

func (s *recordingSink) ApplyEvent(ev Event) { s.evts = append(s.evts, ev) }

// postedBody builds a two_phase_posted CDC body for pixel (x,y), color c,
// with the given transfer id.
func postedBody(x, y uint32, color uint8, id string) []byte {
	pixelID := tbclient.PixelID(x, y).BigInt().String()
	return []byte(fmt.Sprintf(`{
		"timestamp":"1787682583188303701",
		"type":"two_phase_posted",
		"ledger":1,
		"transfer":{
			"id":"%s",
			"amount":"1",
			"pending_id":"6376658857635782656",
			"user_data_128":"79248595801719937611592367840129079151",
			"code":%d,
			"flags":5,
			"timestamp":"1787682583188303701"
		},
		"debit_account":{"id":"%s","code":1000,"flags":2,"timestamp":"1787682583188303701"},
		"credit_account":{"id":"1","code":999,"flags":0,"timestamp":"1787682583188303701"}
	}`, id, uint16(tbclient.TransferCodeClaim)+uint16(color), pixelID))
}

func TestProcessBodyDedupesByTransferID(t *testing.T) {
	sink := &recordingSink{}
	c := NewConsumer(Config{}, sink)

	seen := make(map[string]struct{})
	body := postedBody(3, 4, 9, "6376658857635782657")
	c.processBody(body, seen)
	c.processBody(body, seen) // redelivery: must be dropped
	if len(sink.evts) != 1 {
		t.Fatalf("got %d applies, want 1 (redelivery deduped)", len(sink.evts))
	}

	// A different transfer id is a new event.
	c.processBody(postedBody(5, 6, 2, "6376658857635782699"), seen)
	if len(sink.evts) != 2 {
		t.Fatalf("got %d applies, want 2", len(sink.evts))
	}
	if ev := sink.evts[1]; ev.X != 5 || ev.Y != 6 || ev.Color != 2 {
		t.Fatalf("second event = (%d,%d,%d), want (5,6,2)", ev.X, ev.Y, ev.Color)
	}
}

func TestProcessBodySkipsPoisonPill(t *testing.T) {
	sink := &recordingSink{}
	c := NewConsumer(Config{}, sink)

	seen := make(map[string]struct{})
	c.processBody([]byte("{not json"), seen) // parse failure: acked, never applied
	if len(sink.evts) != 0 {
		t.Fatalf("poison pill reached the sink")
	}
}
