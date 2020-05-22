package state_test

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/LopatkinEvgeniy/clock"
	"github.com/soniah/gosnmp"

	"github.com/lwolf/tcw122b-scripts/internal/snmp"
	"github.com/lwolf/tcw122b-scripts/internal/state"
)

type FakeSunriser struct {
	cl      clock.Clock
	tz      time.Location
	sunrise time.Time
	sunset  time.Time
}

func (fs *FakeSunriser) GetSunriseSunset() (time.Time, time.Time) {
	return fs.sunrise, fs.sunset
}

type mockedSnmp struct {
	values map[string]int64
}

func genPirName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("pir-%d-%v", rand.Int31(), time.Now().String())
}

func (m *mockedSnmp) Get(oids []string) (*gosnmp.SnmpPacket, error) {
	variables := make([]gosnmp.SnmpPDU, 0)
	for _, oid := range oids {
		if val, ok := m.values[oid]; ok {
			variables = append(variables, gosnmp.SnmpPDU{
				Name:  fmt.Sprintf(".%s", oid),
				Type:  gosnmp.Integer,
				Value: val,
			})
		}
	}

	return &gosnmp.SnmpPacket{
		Variables: variables,
	}, nil
}

func (m *mockedSnmp) Set(pdus []gosnmp.SnmpPDU) (*gosnmp.SnmpPacket, error) {
	for _, pdu := range pdus {
		name := pdu.Name
		if pdu.Name[:1] == "." {
			name = pdu.Name[1:]
		}
		m.values[name] = gosnmp.ToBigInt(pdu.Value).Int64()
	}
	return &gosnmp.SnmpPacket{}, nil
}

func NewMockedSnmp(initialState map[string]int64) *mockedSnmp {
	return &mockedSnmp{values: initialState}
}

func makePDU(oid string, value int64) *gosnmp.SnmpPDU {
	return &gosnmp.SnmpPDU{
		Name:  fmt.Sprintf(".%s", oid),
		Type:  gosnmp.Integer,
		Value: value,
	}
}

func getCurrentState(snmp snmp.GetterSetter, values []string) map[string]int64 {
	res, err := snmp.Get(values)
	if err != nil {
		fmt.Printf("failed to get values %v", err)
	}
	data := make(map[string]int64, len(values))
	for _, variable := range res.Variables {
		value := gosnmp.ToBigInt(variable.Value).Int64()
		data[variable.Name[1:]] = value
	}
	return data
}

const (
	fakeStateOid   = "pirStateOid"
	fakeModeOid    = "operationModeOid"
	fakeSwitchOid  = "relayOid"
	fakeTimeoutOid = "timeout"
)

var (
	defaultTestTime = time.Date(2010, time.April, 10, 14, 30, 0, 0, time.UTC)
	defaultSunset   = time.Date(2010, time.April, 10, 20, 00, 0, 0, time.UTC)
	defaultSunrise  = time.Date(2010, time.April, 10, 5, 30, 0, 0, time.UTC)
)

func dumpPir(t *testing.T, pir *state.Pir) (result map[string]int64) {
	t.Helper()
	return map[string]int64{
		pir.Enabled.Oid: pir.Enabled.Value,
		pir.Relay.Oid:   pir.Relay.Value,
	}
}

func expectedLocalState(t *testing.T, state map[string]int64, expected map[string]int64) {
	t.Helper()
	for k, v := range expected {
		if state[k] != v {
			t.Fatalf("expected pir relayState for key=%s to be %d, got %d", k, v, state[k])
		}
	}
}

func expectedRemoteState(t *testing.T, snmp snmp.GetterSetter, values map[string]int64) {
	t.Helper()
	var keys []string
	for k, _ := range values {
		keys = append(keys, k)
	}
	s := getCurrentState(snmp, keys)
	for k, v := range values {
		if s[k] != v {
			t.Fatalf("expected remote relayState for key=%s to be %d, got %d", k, v, s[k])
		}
	}
}

func createTestApp(t *testing.T, pirName string, initialState map[string]int64, cl clock.Clock, useSchedule bool, sunrise time.Time, sunset time.Time) *state.App {
	t.Helper()
	return &state.App{
		Snmp:       NewMockedSnmp(initialState),
		Clock:      cl,
		TZ:         time.UTC,
		TimeoutOid: fakeTimeoutOid,
		Sunriser:   &FakeSunriser{cl, *time.UTC, sunrise, sunset},
		Pirs: map[string]*state.Pir{pirName: state.NewPir(
			pirName,
			fakeStateOid,
			fakeModeOid,
			fakeSwitchOid,
			useSchedule,
		)},
	}
}

func TestNonInteractiveSwitch(t *testing.T) {
	pirName := genPirName(t)
	initialState := map[string]int64{
		fakeStateOid:   0,
		fakeModeOid:    0,
		fakeSwitchOid:  0,
		fakeTimeoutOid: 15,
	}
	testPir := state.NewPir(
		pirName,
		fakeStateOid,
		fakeModeOid,
		fakeSwitchOid,
		false,
	)
	fc := clock.NewFakeClockAt(defaultTestTime)
	app := &state.App{
		Snmp:       NewMockedSnmp(initialState),
		Clock:      fc,
		TZ:         time.UTC,
		Sunriser:   &FakeSunriser{fc, *time.UTC, defaultSunrise, defaultSunset},
		TimeoutOid: fakeTimeoutOid,
		Pirs:       map[string]*state.Pir{pirName: testPir},
	}
	_, err := app.Snmp.Set([]gosnmp.SnmpPDU{*makePDU(fakeStateOid, 1)})
	if err != nil {
		t.Fatalf("failed to patch remote state %v", err)
	}
	// get the relayState and make sure that switch logic works
	err = app.UpdateState()
	if err != nil {
		t.Fatalf("failed to set app relayState %v", err)
	}
	expectedLocalState(t, dumpPir(t, app.PirByName(pirName)), map[string]int64{fakeSwitchOid: 1})
	expectedRemoteState(t, app.Snmp, map[string]int64{fakeSwitchOid: 1})

	// scroll the time to the future
	// advance time gradually, to make sure that
	// background job could kick in any time
	for i := 0; i <= 16; i++ {
		fc.Advance(time.Second)
		app.BackgroundCheck()
	}
	// t.Logf("now %v; pir lastChange %v\n", app.clock.Now(), app.pirByName(pirName).lastChange)
	expectedLocalState(t, dumpPir(t, app.PirByName(pirName)), map[string]int64{fakeSwitchOid: 0})
	expectedRemoteState(t, app.Snmp, map[string]int64{fakeSwitchOid: 0})
}

func TestSchedule(t *testing.T) {
	tests := map[string]struct {
		initialState     map[string]int64
		initialTime      time.Time
		useSchedule      bool
		patchRemoteState []gosnmp.SnmpPDU
		after            time.Duration
		sunrise          time.Time
		sunset           time.Time
		expPirState      map[string]int64
		expRemoteState   map[string]int64
	}{
		"should work anytime with scheduling disabled": {
			initialState: map[string]int64{
				fakeStateOid:   0,
				fakeModeOid:    0,
				fakeSwitchOid:  0,
				fakeTimeoutOid: 15,
			},
			initialTime:      defaultTestTime,
			useSchedule:      false,
			patchRemoteState: []gosnmp.SnmpPDU{*makePDU(fakeStateOid, 1)},
			after:            time.Minute,
			sunrise:          time.Time{},
			sunset:           time.Time{},
			expPirState:      map[string]int64{fakeSwitchOid: 1},
			expRemoteState:   map[string]int64{fakeSwitchOid: 1},
		},
		"should work only during schedule time window": {
			initialState: map[string]int64{
				fakeStateOid:   0,
				fakeModeOid:    0,
				fakeSwitchOid:  0,
				fakeTimeoutOid: 15,
			},
			initialTime:      time.Date(2010, time.April, 10, 20, 30, 0, 0, time.UTC),
			useSchedule:      true,
			sunrise:          defaultSunrise,
			sunset:           defaultSunset,
			patchRemoteState: []gosnmp.SnmpPDU{*makePDU(fakeStateOid, 1)},
			after:            time.Minute,
			expPirState:      map[string]int64{fakeSwitchOid: 1},
			expRemoteState:   map[string]int64{fakeSwitchOid: 1},
		},
		"should not work outside of schedule time window": {
			initialState: map[string]int64{
				fakeStateOid:   0,
				fakeModeOid:    0,
				fakeSwitchOid:  0,
				fakeTimeoutOid: 15,
			},
			initialTime:      defaultTestTime,
			useSchedule:      true,
			sunrise:          defaultSunrise,
			sunset:           defaultSunset,
			patchRemoteState: []gosnmp.SnmpPDU{*makePDU(fakeStateOid, 1)},
			after:            time.Minute,
			expPirState:      map[string]int64{fakeSwitchOid: 0},
			expRemoteState:   map[string]int64{fakeSwitchOid: 0},
		},
	}
	for testName, tc := range tests {
		t.Run(testName, func(t *testing.T) {
			pirName := genPirName(t)
			fc := clock.NewFakeClockAt(tc.initialTime)
			app := createTestApp(t, pirName, tc.initialState, fc, tc.useSchedule, tc.sunrise, tc.sunset)
			// set initial state from the remote
			err := app.UpdateState()
			if err != nil {
				t.Fatalf("failed to set app relayState %v", err)
			}
			// patch remote state
			_, err = app.Snmp.Set(tc.patchRemoteState)
			if err != nil {
				t.Fatalf("failed to patch remote state %v", err)
			}
			// scroll the time to the future
			fc.Advance(tc.after)
			// get the relayState and make sure that switch logic works
			err = app.UpdateState()
			if err != nil {
				t.Fatalf("failed to set app relayState %v", err)
			}
			expectedLocalState(t, dumpPir(t, app.PirByName(pirName)), tc.expPirState)
			expectedRemoteState(t, app.Snmp, tc.expRemoteState)
		})
	}
}

func TestCrossScheduleTransition(t *testing.T) {
	// if switch goes on during schedule enabled timeframe, but
	// then schedule goes to disabled, switch should go offline
	// after default timeout
	// "should handle transition between schedule enabled/disabled": {
	initialState := map[string]int64{
		fakeStateOid:   0,
		fakeModeOid:    0,
		fakeSwitchOid:  0,
		fakeTimeoutOid: 15,
	}
	patchRemoteState := []gosnmp.SnmpPDU{*makePDU(fakeStateOid, 1)}
	expPirState := map[string]int64{fakeSwitchOid: 0}
	expRemoteState := map[string]int64{fakeSwitchOid: 0}

	initialTime := time.Date(2010, time.April, 10, 19, 59, 50, 0, time.UTC)
	pirName := genPirName(t)
	fc := clock.NewFakeClockAt(initialTime)
	app := createTestApp(t, pirName, initialState, fc, true, defaultSunrise, defaultSunset)
	// set initial state from the remote
	err := app.UpdateState()
	if err != nil {
		t.Fatalf("failed to set app relayState %v", err)
	}
	// patch remote state
	_, err = app.Snmp.Set(patchRemoteState)
	if err != nil {
		t.Fatalf("failed to patch remote state %v", err)
	}
	err = app.UpdateState()
	if err != nil {
		t.Fatalf("failed to set app relayState %v", err)
	}
	// advance time gradually, to make sure that
	// background job could kick in any time
	for i := 0; i <= 16; i++ {
		fc.Advance(time.Second)
		app.BackgroundCheck()
	}
	// get the relayState and make sure that switch logic works
	expectedLocalState(t, dumpPir(t, app.PirByName(pirName)), expPirState)
	expectedRemoteState(t, app.Snmp, expRemoteState)

}

func TestWorkflow(t *testing.T) {
	pirName := genPirName(t)
	initialState := map[string]int64{
		fakeStateOid:   0,
		fakeModeOid:    0,
		fakeSwitchOid:  0,
		fakeTimeoutOid: 15,
	}
	fc := clock.NewFakeClockAt(defaultTestTime)
	app := createTestApp(t, pirName, initialState, fc, false, defaultSunrise, defaultSunset)
	steps := []struct {
		name             string
		after            time.Duration
		patchRemoteState []gosnmp.SnmpPDU
		expPirLastChange time.Time
		expPirState      map[string]int64
		expRemoteState   map[string]int64
	}{
		{
			name:             "should switch lights on on new event",
			after:            time.Second,
			patchRemoteState: []gosnmp.SnmpPDU{*makePDU(fakeStateOid, 1)},
			expPirState:      map[string]int64{fakeSwitchOid: 1},
			expRemoteState:   map[string]int64{fakeSwitchOid: 1},
		}, {
			name:             "should not switch off before timeout",
			after:            5 * time.Second,
			patchRemoteState: []gosnmp.SnmpPDU{*makePDU(fakeStateOid, 0)},
			expPirState:      map[string]int64{fakeSwitchOid: 1},
			expRemoteState:   map[string]int64{fakeSwitchOid: 1},
		}, {
			name:             "should reset timeout on PIR events",
			after:            5 * time.Second,
			patchRemoteState: []gosnmp.SnmpPDU{*makePDU(fakeStateOid, 1)},
			expPirState:      map[string]int64{fakeSwitchOid: 1},
			expRemoteState:   map[string]int64{fakeSwitchOid: 1},
		}, {
			name:             "should set timeout",
			after:            5 * time.Second,
			patchRemoteState: []gosnmp.SnmpPDU{*makePDU(fakeStateOid, 0)},
			expPirState:      map[string]int64{fakeSwitchOid: 1},
			expRemoteState:   map[string]int64{fakeSwitchOid: 1},
		}, {
			name:             "should switch off after timeout",
			after:            20 * time.Second,
			patchRemoteState: []gosnmp.SnmpPDU{*makePDU(fakeStateOid, 0)},
			expPirState:      map[string]int64{fakeSwitchOid: 0},
			expRemoteState:   map[string]int64{fakeSwitchOid: 0},
		},
	}
	t.Log("Initialization done, iterating over steps")
	for i, tc := range steps {
		t.Logf("[%d] `%s`", i+1, tc.name)
		// patch remote state
		_, err := app.Snmp.Set(tc.patchRemoteState)
		if err != nil {
			t.Fatalf("failed to patch remote state %v", err)
		}
		// scroll the time to the future
		fc.Advance(tc.after)
		// get the relayState and make sure that switch logic works
		err = app.UpdateState()
		if err != nil {
			t.Fatalf("failed to set app relayState %v", err)
		}
		expectedLocalState(t, dumpPir(t, app.PirByName(pirName)), tc.expPirState)
		expectedRemoteState(t, app.Snmp, tc.expRemoteState)
	}
}
