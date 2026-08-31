package edgedisplay

import (
	"io"
)

type platformEndpoint interface {
	io.ReadWriteCloser
	HostOpener() hostPortOpener
	Restore() error
}
