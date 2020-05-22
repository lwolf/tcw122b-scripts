package main

import (
	"context"
	"flag"
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

	"github.com/lwolf/tcw122b-scripts/internal/state"
	"github.com/lwolf/tcw122b-scripts/internal/sunrise"
)

var (
	Version string
)

const (
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

func main() {
	zerolog.TimeFieldFormat = time.RFC3339
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	snmpHost := flag.String("snmp-remote", "", "IP address of the snmp device")
	snmpPort := flag.Uint("snmp-port", 161, "SNMP port of the device")
	snmpVerbose := flag.Bool("snmp-verbose", false, "Enable verbose logging for the snmp library")
	debug := flag.Bool("debug", false, "Enable debug logging for the app")
	httpAddr := flag.String("addr", "127.0.0.1:8000", "Address and port to serve metrics on")
	timeZone := flag.String("tz", "Europe/Kiev", "Time zone to use")
	lat := flag.Float64("lat", 49.605875, "Latitute for sunset/sunrise estimation")
	lon := flag.Float64("lon", 34.501362, "Longitude for sunset/sunrise estimation")
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
	a := &state.App{
		Snmp:         c,
		Clock:        cl,
		TZ:           loc,
		Sunriser:     sunrise.NewRealSunriser(cl, loc, *lat, *lon),
		Pirs: map[string]*state.Pir{
			"first":  state.NewPir("first", pirAStateOid, pirAModeOid, relayAOid, false),
			"second": state.NewPir("second", pirBStateOid, pirBModeOid, relayBOid, true),
		},
		TimeoutOid: pirTimeout,
	}
	log.Info().Msg("initializing the app state from the remote")
	if err := a.UpdateState(); err != nil {
		log.Fatal().Err(err).Msg("failed to get initial relayState")
	}
	log.Info().Msg("State populated")
	for _, pir := range a.Pirs {
		log.Info().
			Str("pir", pir.Name).
			Dur("timeout", pir.Timeout).
			Int64("enabled", pir.Enabled.Value).
			Int64("relay", pir.Relay.Value).
			Time("turnedOn", pir.TurnedOn).
			Time("lastChange", pir.LastChange).
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
		a.ScheduleBackgroundUpdater(done, time.Millisecond * 300)
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
