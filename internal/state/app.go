package state

import (
	"fmt"
	"os"
	"time"

	"github.com/LopatkinEvgeniy/clock"
	"github.com/VictoriaMetrics/metrics"
	"github.com/rs/zerolog/log"
	"github.com/soniah/gosnmp"

	"github.com/lwolf/tcw122b-scripts/internal/snmp"
	"github.com/lwolf/tcw122b-scripts/internal/sunrise"
)

type relayState int64

const (
	relayOff relayState = 0
	relayOn  relayState = 1
)

type Unit struct {
	Oid   string
	Value int64
}

var (
	// Number of SNMP calls
	requestsTotal = metrics.NewCounter("requests_total")
	// SNMP call duration
	requestDuration = metrics.NewSummary(`requests_duration_seconds`)
)

type App struct {
	Snmp snmp.GetterSetter

	Clock        clock.Clock
	TickInterval time.Duration
	TZ           *time.Location

	Sunriser sunrise.Sunriser
	// time of the SunriseTime
	SunriseTime time.Time
	// time of the SunsetTime
	SunsetTime time.Time
	// SunriseTime and SunsetTime are date specific, therefore need to
	// be updated. `SunsetUpdateDate` used as a cache mark
	// indicating the date of values. if today != SunsetUpdateDate
	// values need to be updated
	SunsetUpdateDate string

	Pirs       map[string]*Pir
	TimeoutOid string
}

func (a *App) PirByName(name string) *Pir {
	return a.Pirs[name]
}

func (a *App) collectOids() (oids []string) {
	oids = append(oids, a.TimeoutOid)
	for _, pir := range a.Pirs {
		oids = append(oids, pir.State.Oid, pir.Enabled.Oid, pir.Relay.Oid)
	}
	return
}

func (a *App) UpdateState() error {
	oids := a.collectOids()
	start := a.Clock.Now()
	requestsTotal.Inc()
	result, err := a.Snmp.Get(oids) // Get() accepts up to g.MAX_OIDS
	if err != nil {
		return fmt.Errorf("failed to Get() err: %v\n", err)
	}
	requestDuration.UpdateDuration(start)
	values := make(map[string]int64)
	for _, variable := range result.Variables {
		value := gosnmp.ToBigInt(variable.Value).Int64()
		values[variable.Name[1:]] = value
	}
	if len(values) != len(oids) {
		return fmt.Errorf("not all values were received: expected %d, got %d\n", len(oids), len(values))
	}
	timeout := time.Duration(values[a.TimeoutOid]) * time.Second
	a.UpdateSchedule()
	for _, pir := range a.Pirs {
		pir.Timeout = timeout
		err = a.UpdatePirState(pir, values)
		if err != nil {
			log.Error().Err(err).Str("pir", pir.Name).Msg("failed to update State")
		}
	}
	return err
}

func (a *App) setRelayState(oid string, newState relayState) error {
	log.Debug().Str("Oid", oid).Int64("State", int64(newState)).Msg("changing remote State")
	// fmt.Printf("[%s] setting Relay relayState to %d\n", Oid, newState)
	v := gosnmp.SnmpPDU{
		Name:  oid,
		Type:  gosnmp.Integer,
		Value: int(newState),
	}
	_, err := a.Snmp.Set([]gosnmp.SnmpPDU{v})
	if err != nil {
		return err
	}
	return nil
}

func (a *App) ScheduleBackgroundUpdater(doneCh <-chan os.Signal) {
	ticker := a.Clock.NewTicker(a.TickInterval)
	for {
		select {
		case <-doneCh:
			ticker.Stop()
			return
		case <-ticker.Chan():
			if err := a.UpdateState(); err != nil {
				log.Error().Err(err).Msg("failed to update app status")
			}
			a.BackgroundCheck()
		}
	}
}

func (a *App) BackgroundCheck() {
	for _, pir := range a.Pirs {
		if !pir.isEnabled() {
			continue
		}
		if relayState(pir.Relay.Value) == relayOn {
			if err := a.MaybeTurnPirOff(pir); err != nil {
				log.Error().Err(err).Str("pir", pir.Name).Msg("failed to run `MaybeTurnPirOff` check")
			}
		}
	}
}

func (a *App) MaybeTurnPirOn(pir *Pir, newState relayState) error {
	// turnOn the lights
	if newState == relayOn {
		err := a.setRelayState(pir.Relay.Oid, newState)
		if err != nil {
			return fmt.Errorf("failed to set Value to the pir %w\n", err)
		}
		pir.TurnedOn = a.Clock.Now()
		pir.LastChange = a.Clock.Now()
		pir.Relay.Value = int64(newState)
		log.Debug().Str("pir", pir.Name).Msg("changed Relay State to relayON")
	}
	return nil
}

func (a *App) MaybeTurnPirOff(pir *Pir) error {
	shouldTurnOff := !pir.LastChange.IsZero() && a.Clock.Now().Sub(pir.LastChange) > pir.Timeout
	if !shouldTurnOff {
		return nil
	}
	err := a.setRelayState(pir.Relay.Oid, relayOff)
	if err != nil {
		return fmt.Errorf("failed to set Value to the pir %w\n", err)
	}
	pir.Relay.Value = int64(relayOff)
	pir.DurationMetric.UpdateDuration(pir.TurnedOn)
	log.Debug().Str("pir", pir.Name).Msg("changing Relay State to relayOff")
	return nil
}

func (a *App) UpdatePirState(pir *Pir, data map[string]int64) error {
	pir.Enabled.Value = data[pir.Enabled.Oid]
	if !pir.isEnabled() {
		log.Debug().Str("pir", pir.Name).Msg("pir is disabled")
		pir.LastChange = time.Time{}
		pir.Relay.Value = int64(relayOff)
		return nil
	}
	newState := relayState(data[pir.State.Oid])
	// expose Relay relayState as a metric
	metrics.GetOrCreateGauge(fmt.Sprintf(`lights_state{pir="%s"}`, pir.Name), func() float64 {
		return float64(newState)
	})
	switch relayState(pir.Relay.Value) {
	case relayOff:
		if !pir.TimeSchedule {
			if err := a.MaybeTurnPirOn(pir, newState); err != nil {
				return err
			}
		} else {
			if !a.ValidTime() {
				return nil
			}
		}
		if err := a.MaybeTurnPirOn(pir, newState); err != nil {
			return err
		}
	case relayOn:
		if newState == relayOff {
			if err := a.MaybeTurnPirOff(pir); err != nil {
				return err
			}
		} else {
			pir.LastChange = a.Clock.Now()
		}
	}
	return nil
}

// ValidTime returns True if current time is between SunsetTime and SunriseTime
func (a *App) ValidTime() bool {
	currentTime := a.Clock.Now().In(a.TZ)
	return currentTime.After(a.SunsetTime) || currentTime.Before(a.SunriseTime)
}

func (a *App) UpdateSchedule() {
	today := a.Clock.Now().Format("2006-01-02")
	if a.SunsetUpdateDate != today || a.SunsetTime.IsZero() || a.SunriseTime.IsZero() {
		srise, sset, err := a.Sunriser.GetSunriseSunset()
		if err != nil {
			log.Error().Err(err).Msg("failed to update SunriseTime data")
		}
		a.SunriseTime = srise
		a.SunsetTime = sset
		a.SunsetUpdateDate = today
		log.Debug().
			Time("sunrise", srise).
			Time("sunset", sset).
			Str("updateDate", today).
			Msg("Schedule updated")
	}
}
