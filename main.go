package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/LopatkinEvgeniy/clock"
	"github.com/VictoriaMetrics/metrics"
	"github.com/soniah/gosnmp"
)

type relayState int64

var (
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

	relayBEnabledFromOid = "1.3.6.1.4.1.38783.2.5.1.1.0" // TEMP1_MIN_VALUE
	relayBEnabledToOid   = "1.3.6.1.4.1.38783.2.5.1.2.0" // TEMP1_MAX_VALUE
	relayBOid            = "1.3.6.1.4.1.38783.3.5.0"
	pirBModeOid          = "1.3.6.1.4.1.38783.2.9.2.0"
	pirBStateOid         = "1.3.6.1.4.1.38783.3.2.0"
)

type app struct {
	snmp         snmpGetterSetter
	clock        clock.Clock
	tickInterval time.Duration

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
			fmt.Printf("failed to update PIR[%s] status %v", pir.name, err)
		}
	}
	return err
}

func (a *app) setRelayState(oid string, newState relayState) error {
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
				fmt.Printf("failed to update app status %v", err)
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
				fmt.Printf("failed to run `MaybeTurnPirOff` check %v\n", err)
			}
		}
	}
}

func (a *app) MaybeTurnPirOn(pir *pir, newState relayState) error {
	if !pir.ValidTime(a.clock) {
		return nil
	}
	// turnOn the lights
	if newState == relayOn {
		err := a.setRelayState(pir.relay.oid, newState)
		if err != nil {
			return fmt.Errorf("failed to set value to the pir %v\n", err)
		}
		pir.turnedOn = a.clock.Now()
		pir.lastChange = a.clock.Now()
		pir.relay.value = int64(newState)
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
		return fmt.Errorf("failed to set value to the pir %v\n", err)
	}
	pir.relay.value = int64(relayOff)
	pir.durationMetric.UpdateDuration(pir.turnedOn)
	return nil
}

func (a *app) UpdatePirState(pir *pir, data map[string]int64) error {
	pir.enabled.value = data[pir.enabled.oid]
	isEnabled := pir.enabled.value == 0
	if !isEnabled {
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
func (p *pir) ValidTime(clock clock.Clock) bool {
	startHour := p.scheduleFrom.value
	endHour := p.scheduleTo.value
	if startHour == 0 && endHour == 0 {
		return true
	}
	currentHour := int64(clock.Now().Hour())
	if startHour > endHour {
		// handles the case when scheduler is configured to work during the night
		// e.g. From 20:00 till 5:00
		return startHour <= currentHour || currentHour < endHour
	} else {
		return startHour <= currentHour && currentHour < endHour
	}
}

func (p *pir) UpdateSchedule(data map[string]int64) {
	p.scheduleTo.value = int2Hour(data[p.scheduleTo.oid])
	p.scheduleFrom.value = int2Hour(data[p.scheduleFrom.oid])
}

// int2Hour returns hour value from the device
func int2Hour(i int64) int64 {
	return i / 10
}

func main() {
	snmpHost := flag.String("snmp-remote", "", "IP address of the snmp device")
	snmpPort := flag.Uint("snmp-port", 161, "SNMP port of the device")
	debug := flag.Bool("debug-debug", false, "Enable debug logging for the snmp library")
	httpAddr := flag.String("addr", "127.0.0.1:8000", "Address and port to serve metrics on")
	flag.Parse()
	if *snmpHost == "" {
		fmt.Printf("`snmp-remote` is required")
		os.Exit(1)
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
	if *debug {
		c.Logger = log.New(os.Stdout, "", 0)
	}
	if err := c.Connect(); err != nil {
		log.Fatalf("Connect() err: %v", err)
	}
	cl := clock.NewRealClock()
	a := &app{
		snmp:         c,
		clock:        cl,
		tickInterval: time.Millisecond * 300,
		pirs: map[string]*pir{
			"first":  NewPir("first", pirAStateOid, pirAModeOid, relayAOid, relayAEnabledFromOid, relayAEnabledToOid),
			"second": NewPir("second", pirBStateOid, pirBModeOid, relayBOid, relayBEnabledFromOid, relayBEnabledToOid),
		},
		timeoutOid: pirTimeout,
	}
	if err := a.updateState(); err != nil {
		log.Fatalf("failed to get initial relayState: %v\n", err)
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
			log.Fatalf("failed to start metrics server %v\n", err)
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
				fmt.Println("got termination signal")
				err := server.Shutdown(context.Background())
				if err != nil {
					fmt.Printf("failed to shutdown metrics server %v\n", err)
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
