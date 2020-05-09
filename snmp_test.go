package main

import (
	"fmt"
	"testing"

	"github.com/jonboulle/clockwork"
	"github.com/soniah/gosnmp"
)

type mockedSnmp struct {
	values map[string]int64
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
	fakeStateOid        = "stateOid"
	fakeModeOid         = "modeOid"
	fakeSwitchOid       = "switchOid"
	fakescheduleFromOid = "scheduleFrom"
	fakescheduleToOid   = "scheduleTo"
	fakeTimeoutOid      = "timeout"
)

func dumpPir(t *testing.T, pir *pir) (result map[string]int64){
	t.Helper()
	var pirMode int64
	if pir.enabled {
		pirMode = 0
	} else {
		pirMode = 4
	}
	return map[string]int64{
		pir.scheduleToOid: pir.scheduleTo,
		pir.scheduleFromOid: pir.scheduleFrom,
		pir.modeOid: pirMode,
		pir.switchOid: int64(pir.state),
	}
}

func expectedLocalState(t *testing.T, state map[string]int64, expected map[string]int64) {
	t.Helper()
	for k, v := range expected {
		if state[k] != v {
			t.Fatalf("expected pir state for key=%s to be %d, got %d", k, v, state[k])
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
			t.Fatalf("expected remote state for key=%s to be %d, got %d", k, v, state[k])
		}
	}
}

func TestLightSwitch(t *testing.T) {
	pirName := "testPir"
	fc := clockwork.NewFakeClock()
	app := &app{
		snmp: NewMockedSnmp(map[string]int64{
			fakeStateOid:        0,
			fakeModeOid:         0,
			fakeSwitchOid:       0,
			fakescheduleFromOid: 0,
			fakescheduleToOid:   0,
			fakeTimeoutOid:      0,
		}),
		clock:      fc,
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
	// change the state of the PIR
	_, err := app.snmp.Set([]gosnmp.SnmpPDU{*makePDU(fakeStateOid, 1)})
	if err != nil {
		t.Fatalf("unexpected error trying to set value %v", err)
	}

	// get the state and make sure that switch logic works
	err = app.setAppState()
	if err != nil {
		t.Fatalf("failed to set app state %v", err)
	}
	pir := app.pirByName(pirName)
	pirState := dumpPir(t, pir)
	expectedLocalState(t, pirState, map[string]int64{fakeSwitchOid: 1})
	expectedRemoteState(t, app.snmp, map[string]int64{fakeSwitchOid: 1})

}

func TestApp(t *testing.T) {
	tests := map[string]struct {
		state    map[string]int64
		expState map[string]int64
	}{
		"should switch lights on on new event": {
			state: map[string]int64{
				"stateOid":  0,
				"modeOid":   0,
				"switchOid": 0,
			},
		},
		"test2": {},
	}
	fc := clockwork.NewFakeClock()
	for testName, tc := range tests {
		t.Run(testName, func(t *testing.T) {
			_ = &app{
				snmp:  NewMockedSnmp(tc.state),
				clock: fc,
			}
		})
	}
}
