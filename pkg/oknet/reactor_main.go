package oknet

import "github.com/samlau0508/imserver/pkg/oklog"

type ReactorMain struct {
	acceptor *Acceptor
	eg       *Engine
	oklog.Log
}

func NewReactorMain(eg *Engine) *ReactorMain {

	return &ReactorMain{
		acceptor: NewAcceptor(eg),
		eg:       eg,
		Log:      oklog.NewOKLog("ReactorMain"),
	}
}

func (m *ReactorMain) Start() error {
	return m.acceptor.Start()
}

func (m *ReactorMain) Stop() error {
	return m.acceptor.Stop()
}
