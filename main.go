package main

import (
	"context"
	"flag"
	"fmt"
	stdlog "log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/LopatkinEvgeniy/clock"
	"github.com/VictoriaMetrics/metrics"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/soniah/gosnmp"
)

type relayState int64

var (
	Version string
	// Number of SNMP calls
	requestsTotal = metrics.NewCounter("requests_total")

	// SNMP call duration
	requestDuration = metrics.NewSummary(`requests_duration_seconds`)
)

type snmpGetterSetter interface {
	Get([]string) (*gosnmp.SnmpPacket, error)
	Set([]gosnmp.SnmpPDU) (*gosnmp.SnmpPacket, error)
}

type Unit struct {
	oid   string
	value int64
}

const (
	relayOff relayState = 0
	relayOn  relayState = 1

	pirTimeout = "1.3.6.1.4.1.38783.2.9.3.0" // RELAY_PULSE_VALUE

	relayAEnabledFromOid = "1.3.6.1.4.1.38783.2.5.1.3.0" // HUMIDITY_MIN_VALUE
	relayAEnabledToOid   = "1.3.6.1.4.1.38783.2.5.1.4.0" // HUMIDITY_MAX_VALUE
	relayAOid            = "1.3.6.1.4.1.38783.3.3.0"
	pirAModeOid          = "1.3.6.1.4.1.38783.2.9.1.0"
	pirAStateOid         = "1.3.6.1.4.1.38783.3.1.0"

	// temporary switch OIDs to test current behaviour
	relayBEnabledFromOid = "1.3.6.1.4.1.38783.2.5.1.2.0" // TEMP1_MIN_VALUE
	relayBEnabledToOid   = "1.3.6.1.4.1.38783.2.5.1.1.0" // TEMP1_MAX_VALUE
	relayBOid            = "1.3.6.1.4.1.38783.3.5.0"
	pirBModeOid          = "1.3.6.1.4.1.38783.2.9.2.0"
	pirBStateOid         = "1.3.6.1.4.1.38783.3.2.0"
)

type app struct {
	snmp         snmpGetterSetter
	clock        clock.Clock
	tickInterval time.Duration
	tz           *time.Location

	pirs       map[string]*pir
	timeoutOid string
}

func (a *app) Print() {
	fmt.Printf("************************\n")
	for pirName, pir := range a.pirs {
		fmt.Printf("pir=%s: %+v\n", pirName, pir)
	}
	fmt.Printf("************************\n")
}

func (a *app) pirByName(name string) *pir {
	return a.pirs[name]
}

func (a *app) collectOids() (oids []string) {
	oids = append(oids, a.timeoutOid)
	for _, pir := range a.pirs {
		oids = append(oids, pir.state.oid, pir.enabled.oid, pir.relay.oid, pir.scheduleFrom.oid, pir.scheduleTo.oid)
	}
	return
}

func (a *app) updateState() error {
	oids := a.collectOids()
	start := a.clock.Now()
	requestsTotal.Inc()
	result, err := a.snmp.Get(oids) // Get() accepts up to g.MAX_OIDS
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
	timeout := time.Duration(values[a.timeoutOid]) * time.Second
	for _, pir := range a.pirs {
		pir.timeout = timeout
		pir.UpdateSchedule(values)
		err = a.UpdatePirState(pir, values)
		if err != nil {
			log.Error().Err(err).Str("pir", pir.name).Msg("failed to update state")
		}
	}
	return err
}

func (a *app) setRelayState(oid string, newState relayState) error {
	log.Debug().Str("oid", oid).Int64("state", int64(newState)).Msg("changing remote state")
	// fmt.Printf("[%s] setting relay relayState to %d\n", oid, newState)
	v := gosnmp.SnmpPDU{
		Name:  oid,
		Type:  gosnmp.Integer,
		Value: int(newState),
	}
	_, err := a.snmp.Set([]gosnmp.SnmpPDU{v})
	if err != nil {
		return err
	}
	return nil
}

func (a *app) ScheduleBackgroundUpdater(doneCh <-chan os.Signal) {
	ticker := a.clock.NewTicker(a.tickInterval)
	for {
		select {
		case <-doneCh:
			ticker.Stop()
			return
		case <-ticker.Chan():
			if err := a.updateState(); err != nil {
				log.Error().Err(err).Msg("failed to update app status")
			}
			a.BackgroundCheck()
		}
	}
}

func (a *app) BackgroundCheck() {
	for _, pir := range a.pirs {
		if !pir.Enabled() {
			continue
		}
		if relayState(pir.relay.value) == relayOn {
			if err := a.MaybeTurnPirOff(pir); err != nil {
				log.Error().Err(err).Str("pir", pir.name).Msg("failed to run `MaybeTurnPirOff` check")
			}
		}
	}
}

func (a *app) MaybeTurnPirOn(pir *pir, newState relayState) error {
	if !pir.ValidTime(a.clock, a.tz) {
		return nil
	}
	// turnOn the lights
	if newState == relayOn {
		err := a.setRelayState(pir.relay.oid, newState)
		if err != nil {
			return fmt.Errorf("failed to set value to the pir %w\n", err)
		}
		pir.turnedOn = a.clock.Now()
		pir.lastChange = a.clock.Now()
		pir.relay.value = int64(newState)
		log.Debug().Str("pir", pir.name).Msg("changed relay state to relayON")
	}
	return nil
}

func (a *app) MaybeTurnPirOff(pir *pir) error {
	shouldTurnOff := !pir.lastChange.IsZero() && a.clock.Now().Sub(pir.lastChange) > pir.timeout
	if !shouldTurnOff {
		return nil
	}
	err := a.setRelayState(pir.relay.oid, relayOff)
	if err != nil {
		return fmt.Errorf("failed to set value to the pir %w\n", err)
	}
	pir.relay.value = int64(relayOff)
	pir.durationMetric.UpdateDuration(pir.turnedOn)
	log.Debug().Str("pir", pir.name).Msg("changing relay state to relayOff")
	return nil
}

func (a *app) UpdatePirState(pir *pir, data map[string]int64) error {
	pir.enabled.value = data[pir.enabled.oid]
	isEnabled := pir.enabled.value == 0
	if !isEnabled {
		log.Debug().Str("pir", pir.name).Msg("pir is disabled")
		pir.lastChange = time.Time{}
		pir.relay.value = int64(relayOff)
		return nil
	}
	newState := relayState(data[pir.state.oid])
	// expose relay relayState as a metric
	metrics.GetOrCreateGauge(fmt.Sprintf(`lights_state{pir="%s"}`, pir.name), func() float64 {
		return float64(newState)
	})
	switch relayState(pir.relay.value) {
	case relayOff:
		if err := a.MaybeTurnPirOn(pir, newState); err != nil {
			return err
		}
	case relayOn:
		if newState == relayOff {
			if err := a.MaybeTurnPirOff(pir); err != nil {
				return err
			}
		} else {
			pir.lastChange = a.clock.Now()
		}
	}
	return nil
}

type pir struct {
	name string
	// remote state of the PIR
	state Unit
	// PIR is enabled if pin set to 0 (MANUAL)
	enabled Unit
	// current relayState of the relay
	relay Unit
	// timestamp when relayState changed to ON
	// used for tracking a single session duration
	turnedOn   time.Time
	lastChange time.Time
	timeout    time.Duration

	scheduleFrom Unit
	scheduleTo   Unit

	// Duration of lights being on
	durationMetric *metrics.Summary
}

func NewPir(name, stateOid, modeOid, switchOid, scheduleFromOid, scheduleToOid string) *pir {
	return &pir{
		name:           name,
		enabled:        Unit{oid: modeOid, value: 1},
		state:          Unit{oid: stateOid},
		relay:          Unit{oid: switchOid},
		scheduleFrom:   Unit{oid: scheduleFromOid},
		scheduleTo:     Unit{oid: scheduleToOid},
		durationMetric: metrics.NewSummary(fmt.Sprintf(`ligths_state_on_seconds{pir="%s"}`, name)),
	}
}

func (p *pir) Enabled() bool {
	// Corresponding remote flag has following values:
	// 		MANUAL = 0
	// 		DIGITALINPUT1 = 4  # PIR1
	// 		DIGITALINPUT2 = 8  # PIR2
	// We're interested in the Manual mode, which corresponds to
	// the enabled remote control
	return p.enabled.value == 0
}

// ValidTime returns True if neither From time nor To time is set
// or if current hour is in between From and To
func (p *pir) ValidTime(clock clock.Clock, tz *time.Location) bool {
	startHour := p.scheduleFrom.value
	endHour := p.scheduleTo.value
	if startHour == 0 && endHour == 0 {
		return true
	}
	currentHour := int64(clock.Now().In(tz).Hour())
	if startHour > endHour {
		// handles the case when scheduler is configured to work during the night
		// e.g. From 20:00 till 5:00
		return startHour <= currentHour || currentHour < endHour
	} else {
		return startHour <= currentHour && currentHour < endHour
	}
}

func (p *pir) UpdateSchedule(data map[string]int64) {
	newFrom := int2Hour(data[p.scheduleFrom.oid])
	newTo := int2Hour(data[p.scheduleTo.oid])

	if p.scheduleFrom.value != newFrom {
		log.Debug().
			Str("pir", p.name).
			Int64("from", newFrom).
			Msg("updating the schedule")
		p.scheduleFrom.value = newFrom
	}

	if p.scheduleTo.value != newTo {
		log.Debug().
			Str("pir", p.name).
			Int64("to", newTo).
			Msg("updating the schedule")
		p.scheduleTo.value = newTo
	}
}

// int2Hour returns hour value from the device
func int2Hour(i int64) int64 {
	return i / 10
}

func main() {
	zerolog.TimeFieldFormat = time.RFC3339
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	snmpHost := flag.String("snmp-remote", "", "IP address of the snmp device")
	snmpPort := flag.Uint("snmp-port", 161, "SNMP port of the device")
	snmpVerbose := flag.Bool("snmp-verbose", false, "Enable verbose logging for the snmp library")
	debug := flag.Bool("debug", false, "Enable debug logging for the app")
	httpAddr := flag.String("addr", "127.0.0.1:8000", "Address and port to serve metrics on")
	timeZone := flag.String("tz", "Europe/Kiev", "Time zone to use")
	flag.Parse()
	loc, err := time.LoadLocation(*timeZone)
	if err != nil {
		log.Fatal().Err(err).Str("tz", *timeZone).Msg("failed to parse time zone")
	}
	log.Info().
		Time("local time", time.Now().In(loc)).
		Str("version", Version).
		Msg("Starting app")

	// Default level for this example is info, unless debug flag is present
	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	if *debug {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}

	if *snmpHost == "" {
		log.Fatal().Msg("`snmp-remote` is required")
	}
	c := &gosnmp.GoSNMP{
		Port:               uint16(*snmpPort),
		Target:             *snmpHost,
		Transport:          "udp",
		Community:          "private",
		Version:            gosnmp.Version2c,
		Timeout:            time.Duration(1) * time.Second,
		Retries:            3,
		ExponentialTimeout: true,
		MaxOids:            gosnmp.MaxOids,
	}
	if *snmpVerbose {
		c.Logger = stdlog.New(os.Stdout, "", 0)
	}
	log.Info().Str("host", *snmpHost).Msg("connecting to the snmp host")
	if err := c.Connect(); err != nil {
		log.Fatal().Err(err).Msg("failed to Connect()")
	}
	cl := clock.NewRealClock()
	a := &app{
		snmp:         c,
		clock:        cl,
		tz:           loc,
		tickInterval: time.Millisecond * 300,
		pirs: map[string]*pir{
			"first":  NewPir("first", pirAStateOid, pirAModeOid, relayAOid, relayAEnabledFromOid, relayAEnabledToOid),
			"second": NewPir("second", pirBStateOid, pirBModeOid, relayBOid, relayBEnabledFromOid, relayBEnabledToOid),
		},
		timeoutOid: pirTimeout,
	}
	log.Info().Msg("initializing the app state from the remote")
	if err := a.updateState(); err != nil {
		log.Fatal().Err(err).Msg("failed to get initial relayState")
	}
	log.Info().Msg("State populated")
	for _, pir := range a.pirs {
		log.Info().
			Str("pir", pir.name).
			Dur("timeout", pir.timeout).
			Int64("enabled", pir.enabled.value).
			Int64("relay", pir.relay.value).
			Time("turnedOn", pir.turnedOn).
			Time("lastChange", pir.lastChange).
			Int64("scheduleFrom", pir.scheduleFrom.value).
			Int64("scheduleTo", pir.scheduleTo.value).
			Msg("pir state")
	}

	var wg sync.WaitGroup
	done := make(chan os.Signal)
	signal.Notify(done, syscall.SIGHUP, syscall.SIGUSR2, syscall.SIGINT, syscall.SIGQUIT)

	server := http.Server{Addr: *httpAddr}
	wg.Add(1)
	go func() {
		// Expose the registered metrics at `/metrics` path.
		http.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
			metrics.WritePrometheus(w, true)
		})
		err := server.ListenAndServe()
		if err != nil {
			log.Fatal().Err(err).Msg("failed to ListenAndServe metrics server")
		}
		wg.Done()
	}()

	wg.Add(1)
	go func() {
		a.ScheduleBackgroundUpdater(done)
		wg.Done()
	}()

	wg.Add(1)
	go func() {
		for {
			select {
			case <-done:
				log.Info().Msg("got termination signal")
				err := server.Shutdown(context.Background())
				if err != nil {
					log.Fatal().Err(err).Msg("failed to shutdown metrics server")
				}
				wg.Done()
				return
			}
		}
	}()
	<-done // Blocks here until either SIGINT or SIGTERM is received.
	wg.Wait()
	defer c.Conn.Close()
}
