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

	"github.com/VictoriaMetrics/metrics"
	"github.com/soniah/gosnmp"
)

type state int64

var (
	// Number of SNMP calls
	requestsTotal = metrics.NewCounter("requests_total")

	// SNMP call duration
	requestDuration = metrics.NewSummary(`requests_duration_seconds`)
)

const (
	stateOff state = 0
	stateOn  state = 1

	pirTimeout = "1.3.6.1.4.1.38783.2.9.3.0" // RELAY_PULSE_VALUE

	relay1WorkFrom = "1.3.6.1.4.1.38783.2.5.1.3.0" // HUMIDITY_MIN_VALUE
	relay1WorkTo   = "1.3.6.1.4.1.38783.2.5.1.4.0" // HUMIDITY_MAX_VALUE
	pir1Mode       = "1.3.6.1.4.1.38783.2.9.1.0"
	relay1Id       = "1.3.6.1.4.1.38783.3.3.0"
	pir1State      = "1.3.6.1.4.1.38783.3.1.0"

	relay2WorkFrom = "1.3.6.1.4.1.38783.2.5.1.1.0" // TEMP1_MIN_VALUE
	relay2WorkTo   = "1.3.6.1.4.1.38783.2.5.1.2.0" // TEMP1_MAX_VALUE
	pir2Mode       = "1.3.6.1.4.1.38783.2.9.2.0"
	relay2Id       = "1.3.6.1.4.1.38783.3.5.0"
	pir2State      = "1.3.6.1.4.1.38783.3.2.0"
)

type app struct {
	snmp *gosnmp.GoSNMP

	pir1 *pir
	pir2 *pir
}

func (a *app) setAppState() error {
	oids := []string{
		pir1State, pir1Mode, relay1Id, relay1WorkFrom, relay1WorkTo,
		pir2State, pir2Mode, relay2Id, relay2WorkFrom, relay2WorkTo,
		pirTimeout,
	}
	start := time.Now()
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
	timeout := time.Duration(values[pirTimeout]) * time.Second
	a.pir1.timeout = timeout
	a.pir1.schedule.UpdateState(values)
	a.pir2.timeout = timeout
	a.pir2.schedule.UpdateState(values)
	return nil
}

func (a *app) setRelayState(oid string, newState state) error {
	fmt.Printf("[%s] setting relay state to %d\n", oid, newState)
	v := gosnmp.SnmpPDU{
		Name:  oid,
		Type:  gosnmp.Integer,
		Value: newState,
	}
	res, err := a.snmp.Set([]gosnmp.SnmpPDU{v})
	if err != nil {
		return err
	}
	fmt.Printf("relay state set %v\n", res)
	return nil
}

type pir struct {
	name       string
	enabled    bool
	state      state
	lastChange time.Time
	schedule   *pirSchedule
	timeout    time.Duration

	turnedOn time.Time

	stateOid  string
	modeOid   string
	switchOid string

	// Duration of lights being on
	durationMetric *metrics.Summary
}

func NewPir(name string, stateOid string, modeOid string, switchOid string) *pir {
	return &pir{
		name:           name,
		enabled:        true,
		stateOid:       stateOid,
		modeOid:        modeOid,
		switchOid:      switchOid,
		durationMetric: metrics.NewSummary(fmt.Sprintf(`ligths_on_seconds{pir="%s"}`, name)),
	}
}

func (a *app) UpdatePirState(pir *pir, data map[string]int64) error {
	pir.enabled = data[pir.modeOid] == 0
	if !pir.enabled {
		pir.lastChange = time.Time{}
		pir.state = stateOff
		return nil
	}
	newState := state(data[pir.stateOid])
	switch pir.state {
	case stateOff:
		if !pir.schedule.ValidTime() {
			return nil
		}
		// turnOn the lights
		if newState == stateOn {
			err := a.setRelayState(pir.switchOid, newState)
			if err != nil {
				return fmt.Errorf("failed to set value to the pir %v\n", err)
			}
			pir.turnedOn = time.Now()
			pir.state = newState
		}
		// reset the clock
		pir.lastChange = time.Time{}
	case stateOn:
		if newState == stateOff {
			pir.lastChange = time.Now()
			shouldTurnOff := !pir.lastChange.IsZero() && time.Now().Sub(pir.lastChange) > pir.timeout
			if shouldTurnOff {
				err := a.setRelayState(pir.switchOid, stateOff)
				if err != nil {
					return fmt.Errorf("failed to set value to the pir %v\n", err)
				}
				pir.state = stateOff
				pir.durationMetric.UpdateDuration(pir.turnedOn)
			}
		} else {
			pir.lastChange = time.Time{}
		}
	}
	return nil
}

type pirSchedule struct {
	From int64
	To   int64

	fromOid string
	toOid   string
}

func NewPirSchedule(fromOid string, toOid string) *pirSchedule {
	return &pirSchedule{
		fromOid: fromOid,
		toOid:   toOid,
	}
}

// ValidTime returns True if neither From time nor To time is set
// or if current hour is in between From and To
func (ps *pirSchedule) ValidTime() bool {
	if ps.From == 0 && ps.To == 0 {
		return true
	}
	currentHour := int64(time.Now().Hour())
	return ps.From <= currentHour && currentHour < ps.To
}

func (ps *pirSchedule) UpdateState(data map[string]int64) {
	ps.To = int2Hour(data[ps.toOid])
	ps.From = int2Hour(data[ps.fromOid])
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
		snmp: c,
		pir1: NewPir("short", pir1State, pir1Mode, relay1Id),
		pir2: NewPir("long", pir2State, pir2Mode, relay2Id),
	}
	a.pir1.schedule = NewPirSchedule(relay1WorkFrom, relay1WorkTo)
	a.pir2.schedule = NewPirSchedule(relay2WorkFrom, relay2WorkTo)
	err = a.setAppState()
	if err != nil {
		log.Fatalf("failed to get initial state: %v\n", err)
	}

	var wg sync.WaitGroup
	ticker := time.NewTicker(300 * time.Millisecond)
	done := make(chan os.Signal)
	signal.Notify(done, syscall.SIGHUP, syscall.SIGUSR2, syscall.SIGINT, syscall.SIGQUIT)

	server := http.Server{Addr: *httpAddr}
	wg.Add(1)
	go func(){
		// Expose the registered metrics at `/metrics` path.
		http.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
			metrics.WritePrometheus(w, true)
		})
		err = server.ListenAndServe()
		if err != nil {
			log.Fatalf("failed to start metrics server %v\n", err)
		}
		wg.Done()
	} ()


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
					fmt.Printf("failed to query remote state %v\n", err)
				}
			}
		}
	}()
	<-done // Blocks here until either SIGINT or SIGTERM is received.
	wg.Wait()
	defer c.Conn.Close()
}
