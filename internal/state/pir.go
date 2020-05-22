package state

import (
	"fmt"
	"time"

	"github.com/VictoriaMetrics/metrics"
)

type Pir struct {
	Name string
	// remote State of the PIR
	State Unit
	// PIR is Enabled if pin set to 0 (MANUAL)
	Enabled Unit
	// current relayState of the Relay
	Relay Unit
	// timestamp when relayState changed to ON
	// used for tracking a single session duration
	TurnedOn   time.Time
	LastChange time.Time
	Timeout    time.Duration

	// indicates whether time schedule is Enabled
	TimeSchedule bool

	// Duration of lights being on
	DurationMetric *metrics.Summary
}


func NewPir(name, stateOid, modeOid, switchOid string, timeSchedule bool) *Pir {
	return &Pir{
		Name:           name,
		Enabled:        Unit{Oid: modeOid, Value: 1},
		State:          Unit{Oid: stateOid},
		Relay:          Unit{Oid: switchOid},
		TimeSchedule:   timeSchedule,
		DurationMetric: metrics.NewSummary(fmt.Sprintf(`ligths_state_on_seconds{pir="%s"}`, name)),
	}
}

func (p *Pir) isEnabled() bool {
	// Corresponding remote flag has following values:
	// 		MANUAL = 0
	// 		DIGITALINPUT1 = 4  # PIR1
	// 		DIGITALINPUT2 = 8  # PIR2
	// We're interested in the Manual mode, which corresponds to
	// the Enabled remote control
	return p.Enabled.Value == 0
}
