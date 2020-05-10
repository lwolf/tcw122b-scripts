package main

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/LopatkinEvgeniy/clock"
	"github.com/soniah/gosnmp"
)

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

func getCurrentState(snmp snmpGetterSetter, values []string) map[string]int64 {
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
	fakeStateOid        = "pirStateOid"
	fakeModeOid         = "operationModeOid"
	fakeSwitchOid       = "relayOid"
	fakescheduleFromOid = "scheduleFrom"
	fakescheduleToOid   = "scheduleTo"
	fakeTimeoutOid      = "timeout"
)

func dumpPir(t *testing.T, pir *pir) (result map[string]int64) {
	t.Helper()
	return map[string]int64{
		pir.scheduleTo.oid:   pir.scheduleTo.value,
		pir.scheduleFrom.oid: pir.scheduleFrom.value,
		pir.enabled.oid:      pir.enabled.value,
		pir.relay.oid:        pir.relay.value,
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

func expectedRemoteState(t *testing.T, snmp snmpGetterSetter, values map[string]int64) {
	t.Helper()
	var keys []string
	for k, _ := range values {
		keys = append(keys, k)
	}
	state := getCurrentState(snmp, keys)
	for k, v := range values {
		if state[k] != v {
			t.Fatalf("expected remote relayState for key=%s to be %d, got %d", k, v, state[k])
		}
	}
}

func createTestApp(t *testing.T, pirName string, initialState map[string]int64, cl clock.Clock) *app {
	t.Helper()
	return &app{
		snmp:       NewMockedSnmp(initialState),
		clock:      cl,
		timeoutOid: fakeTimeoutOid,
		pirs: map[string]*pir{pirName: NewPir(
			pirName,
			fakeStateOid,
			fakeModeOid,
			fakeSwitchOid,
			fakescheduleFromOid,
			fakescheduleToOid,
		)},
	}
}

func TestNonInteractiveSwitch(t *testing.T) {
	pirName := genPirName(t)
	initialState := map[string]int64{
		fakeStateOid:        0,
		fakeModeOid:         0,
		fakeSwitchOid:       0,
		fakescheduleFromOid: 0,
		fakescheduleToOid:   0,
		fakeTimeoutOid:      15,
	}
	initialTime := time.Date(2010, time.April, 10, 14, 30, 0, 0, time.UTC)
	testPir := NewPir(
		pirName,
		fakeStateOid,
		fakeModeOid,
		fakeSwitchOid,
		fakescheduleFromOid,
		fakescheduleToOid,
	)
	fc := clock.NewFakeClockAt(initialTime)
	app := &app{
		snmp:       NewMockedSnmp(initialState),
		clock:      fc,
		timeoutOid: fakeTimeoutOid,
		pirs:       map[string]*pir{pirName: testPir},
	}
	_, err := app.snmp.Set([]gosnmp.SnmpPDU{*makePDU(fakeStateOid, 1)})
	if err != nil {
		t.Fatalf("failed to patch remote state %v", err)
	}
	// get the relayState and make sure that switch logic works
	err = app.setAppState()
	if err != nil {
		t.Fatalf("failed to set app relayState %v", err)
	}
	expectedLocalState(t, dumpPir(t, app.pirByName(pirName)), map[string]int64{fakeSwitchOid: 1})
	expectedRemoteState(t, app.snmp, map[string]int64{fakeSwitchOid: 1})

	// scroll the time to the future
	// advance time gradually, to make sure that
	// background job could kick in any time
	// TODO: remove after proper background worker implemented
	for i := 0; i <= 16; i++ {
		fc.Advance(time.Second)
		app.BackgroundCheck()
	}
	// t.Logf("now %v; pir lastChange %v\n", app.clock.Now(), app.pirByName(pirName).lastChange)
	expectedLocalState(t, dumpPir(t, app.pirByName(pirName)), map[string]int64{fakeSwitchOid: 0})
	expectedRemoteState(t, app.snmp, map[string]int64{fakeSwitchOid: 0})
}

func TestSchedule(t *testing.T) {
	tests := map[string]struct {
		initialState     map[string]int64
		initialTime      time.Time
		patchRemoteState []gosnmp.SnmpPDU
		after            time.Duration
		expPirState      map[string]int64
		expRemoteState   map[string]int64
	}{
		"should work anytime with scheduling disabled": {
			initialState: map[string]int64{
				fakeStateOid:        0,
				fakeModeOid:         0,
				fakeSwitchOid:       0,
				fakescheduleFromOid: 0,
				fakescheduleToOid:   0,
				fakeTimeoutOid:      15,
			},
			initialTime:      time.Date(2010, time.April, 10, 14, 30, 0, 0, time.UTC),
			patchRemoteState: []gosnmp.SnmpPDU{*makePDU(fakeStateOid, 1)},
			after:            time.Minute,
			expPirState:      map[string]int64{fakeSwitchOid: 1},
			expRemoteState:   map[string]int64{fakeSwitchOid: 1},
		},
		"should work only during schedule time window": {
			initialState: map[string]int64{
				fakeStateOid:        0,
				fakeModeOid:         0,
				fakeSwitchOid:       0,
				fakescheduleFromOid: 200,
				fakescheduleToOid:   50,
				fakeTimeoutOid:      15,
			},
			initialTime:      time.Date(2010, time.April, 10, 20, 30, 0, 0, time.UTC),
			patchRemoteState: []gosnmp.SnmpPDU{*makePDU(fakeStateOid, 1)},
			after:            time.Minute,
			expPirState:      map[string]int64{fakeSwitchOid: 1},
			expRemoteState:   map[string]int64{fakeSwitchOid: 1},
		},
		"should not work outside of schedule time window": {
			initialState: map[string]int64{
				fakeStateOid:        0,
				fakeModeOid:         0,
				fakeSwitchOid:       0,
				fakescheduleFromOid: 200,
				fakescheduleToOid:   50,
				fakeTimeoutOid:      15,
			},
			initialTime:      time.Date(2010, time.April, 10, 14, 30, 0, 0, time.UTC),
			patchRemoteState: []gosnmp.SnmpPDU{*makePDU(fakeStateOid, 1)},
			after:            time.Minute,
			expPirState:      map[string]int64{fakeSwitchOid: 0},
			expRemoteState:   map[string]int64{fakeSwitchOid: 0},
		},
		// if switch goes on during schedule enabled timeframe, but
		// then schedule goes to disabled, switch should go offline
		// after default timeout
		// "should handle transition between schedule enabled/disabled": {
		// 	initialState: map[string]int64{
		// 		fakeStateOid:        0,
		// 		fakeModeOid:         0,
		// 		fakeSwitchOid:       0,
		// 		fakescheduleFromOid: 200,
		// 		fakescheduleToOid:   50,
		// 		fakeTimeoutOid:      15,
		// 	},
		// 	initialTime:      time.Date(2010, time.April, 10, 19, 59, 50, 0, time.UTC),
		// 	patchRemoteState: []gosnmp.SnmpPDU{*makePDU(fakeStateOid, 1)},
		// 	after:            time.Minute,
		// 	expPirState:      map[string]int64{fakeSwitchOid: 0},
		// 	expRemoteState:   map[string]int64{fakeSwitchOid: 0},
		// },
	}
	for testName, tc := range tests {
		t.Run(testName, func(t *testing.T) {
			pirName := genPirName(t)
			fc := clock.NewFakeClockAt(tc.initialTime)
			app := createTestApp(t, pirName, tc.initialState, fc)
			// set initial state from the remote
			err := app.setAppState()
			if err != nil {
				t.Fatalf("failed to set app relayState %v", err)
			}
			// patch remote state
			_, err = app.snmp.Set(tc.patchRemoteState)
			if err != nil {
				t.Fatalf("failed to patch remote state %v", err)
			}
			// scroll the time to the future
			fc.Advance(tc.after)
			// get the relayState and make sure that switch logic works
			err = app.setAppState()
			if err != nil {
				t.Fatalf("failed to set app relayState %v", err)
			}
			expectedLocalState(t, dumpPir(t, app.pirByName(pirName)), tc.expPirState)
			expectedRemoteState(t, app.snmp, tc.expRemoteState)
		})
	}
}

func TestWorkflow(t *testing.T) {
	pirName := genPirName(t)
	initialState := map[string]int64{
		fakeStateOid:        0,
		fakeModeOid:         0,
		fakeSwitchOid:       0,
		fakescheduleFromOid: 0,
		fakescheduleToOid:   0,
		fakeTimeoutOid:      15,
	}
	initialTime := time.Date(2010, time.April, 10, 14, 30, 0, 0, time.UTC)
	fc := clock.NewFakeClockAt(initialTime)
	app := createTestApp(t, pirName, initialState, fc)
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
		_, err := app.snmp.Set(tc.patchRemoteState)
		if err != nil {
			t.Fatalf("failed to patch remote state %v", err)
		}
		// scroll the time to the future
		fc.Advance(tc.after)
		// get the relayState and make sure that switch logic works
		err = app.setAppState()
		if err != nil {
			t.Fatalf("failed to set app relayState %v", err)
		}
		expectedLocalState(t, dumpPir(t, app.pirByName(pirName)), tc.expPirState)
		expectedRemoteState(t, app.snmp, tc.expRemoteState)
	}
}
