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
	snmp  snmpGetterSetter
	clock clock.Clock

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
		oids = append(oids, pir.pirStateOid, pir.operationModeOid, pir.relayOid, pir.scheduleFromOid, pir.scheduleToOid)
	}
	return
}

func (a *app) setAppState() error {
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
		Value: int64(newState),
	}
	_, err := a.snmp.Set([]gosnmp.SnmpPDU{v})
	if err != nil {
		return err
	}
	return nil
}

type pir struct {
	name string
	// PIR is enabled if pin set to 0 (MANUAL)
	enabled bool
	// current relayState of the relay
	relayState relayState
	lastChange time.Time
	timeout    time.Duration

	scheduleFrom int64
	scheduleTo   int64
	// timestamp when relayState changed to ON
	// used for tracking a single session duration
	turnedOn time.Time

	pirStateOid      string
	operationModeOid string
	relayOid         string
	scheduleFromOid  string
	scheduleToOid    string

	// Duration of lights being on
	durationMetric *metrics.Summary
}

func NewPir(name, stateOid, modeOid, switchOid, scheduleFromOid, scheduleToOid string) *pir {
	return &pir{
		name:             name,
		enabled:          true,
		pirStateOid:      stateOid,
		operationModeOid: modeOid,
		relayOid:         switchOid,
		scheduleFromOid:  scheduleFromOid,
		scheduleToOid:    scheduleToOid,
		durationMetric:   metrics.NewSummary(fmt.Sprintf(`ligths_state_on_seconds{pir="%s"}`, name)),
	}
}

// ValidTime returns True if neither From time nor To time is set
// or if current hour is in between From and To
func (p *pir) ValidTime(clock clock.Clock) bool {
	if p.scheduleFrom == 0 && p.scheduleTo == 0 {
		return true
	}
	currentHour := int64(clock.Now().Hour())
	if p.scheduleFrom > p.scheduleTo {
		// handles the case when scheduler is configured to work during the night
		// e.g. From 20:00 till 5:00
		return p.scheduleFrom <= currentHour || currentHour < p.scheduleTo
	} else {
		return p.scheduleFrom <= currentHour && currentHour < p.scheduleTo
	}
}

func (p *pir) UpdateSchedule(data map[string]int64) {
	p.scheduleTo = int2Hour(data[p.scheduleToOid])
	p.scheduleFrom = int2Hour(data[p.scheduleFromOid])
}

func (a *app) UpdatePirState(pir *pir, data map[string]int64) error {
	pir.enabled = data[pir.operationModeOid] == 0
	if !pir.enabled {
		pir.lastChange = time.Time{}
		pir.relayState = relayOff
		return nil
	}
	newState := relayState(data[pir.pirStateOid])
	// expose relay relayState as a metric
	metrics.GetOrCreateGauge(fmt.Sprintf(`lights_state{pir="%s"}`, pir.name), func() float64 {
		return float64(newState)
	})
	switch pir.relayState {
	case relayOff:
		if !pir.ValidTime(a.clock) {
			return nil
		}
		// turnOn the lights
		if newState == relayOn {
			err := a.setRelayState(pir.relayOid, newState)
			if err != nil {
				return fmt.Errorf("failed to set value to the pir %v\n", err)
			}
			pir.turnedOn = a.clock.Now()
			pir.relayState = newState
			go func() {
				ticker := time.NewTicker(time.Second)
				for {
					select {
					case <-ticker.C:
						if !pir.enabled {
							return
						}
						// TODO: if shouldTurnOff { turnOff and return }
					}
				}
			}()
		}
		// reset the clock
		pir.lastChange = time.Time{}
	case relayOn:
		if newState == relayOff {
			shouldTurnOff := !pir.lastChange.IsZero() && a.clock.Now().Sub(pir.lastChange) > pir.timeout
			if shouldTurnOff {
				err := a.setRelayState(pir.relayOid, relayOff)
				if err != nil {
					return fmt.Errorf("failed to set value to the pir %v\n", err)
				}
				pir.relayState = relayOff
				pir.durationMetric.UpdateDuration(pir.turnedOn)
				return nil
			}
			pir.lastChange = a.clock.Now()
		} else {
			pir.lastChange = time.Time{}
		}
	}
	return nil
}

// int2Hour returns hour value from the device
func int2Hour(i int64) int64 {
	return i / 10
}

func main() {
	snmpHost := flag.String("snmp-remote", "", "IP address of the snmp device")
	snmpPort := flag.Uint("snmp-port", 161, "SNMP port of the device")
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
		Community:          "public",
		Version:            gosnmp.Version2c,
		Timeout:            time.Duration(1) * time.Second,
		Retries:            3,
		ExponentialTimeout: true,
		MaxOids:            gosnmp.MaxOids,
	}
	err := c.Connect()
	if err != nil {
		log.Fatalf("Connect() err: %v", err)
	}
	a := &app{
		snmp:  c,
		clock: clock.NewRealClock(),
		pirs: map[string]*pir{
			"first":  NewPir("first", pirAStateOid, pirAModeOid, relayAOid, relayAEnabledFromOid, relayAEnabledToOid),
			"second": NewPir("second", pirBStateOid, pirBModeOid, relayBOid, relayBEnabledFromOid, relayBEnabledToOid),
		},
		timeoutOid: pirTimeout,
	}
	err = a.setAppState()
	if err != nil {
		log.Fatalf("failed to get initial relayState: %v\n", err)
	}

	var wg sync.WaitGroup
	ticker := time.NewTicker(300 * time.Millisecond)
	done := make(chan os.Signal)
	signal.Notify(done, syscall.SIGHUP, syscall.SIGUSR2, syscall.SIGINT, syscall.SIGQUIT)

	server := http.Server{Addr: *httpAddr}
	wg.Add(1)
	go func() {
		// Expose the registered metrics at `/metrics` path.
		http.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
			metrics.WritePrometheus(w, true)
		})
		err = server.ListenAndServe()
		if err != nil {
			log.Fatalf("failed to start metrics server %v\n", err)
		}
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
			case <-ticker.C:
				err = a.setAppState()
				if err != nil {
					fmt.Printf("failed to query remote relayState %v\n", err)
				}
			}
		}
	}()
	<-done // Blocks here until either SIGINT or SIGTERM is received.
	wg.Wait()
	defer c.Conn.Close()
}
