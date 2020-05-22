package snmp

import "github.com/soniah/gosnmp"

type GetterSetter interface {
	Get([]string) (*gosnmp.SnmpPacket, error)
	Set([]gosnmp.SnmpPDU) (*gosnmp.SnmpPacket, error)
}
