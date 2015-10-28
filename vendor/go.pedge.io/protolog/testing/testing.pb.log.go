// Code generated by protoc-gen-protolog
// source: testing/testing.proto
// DO NOT EDIT!

package protolog_testing

import "go.pedge.io/protolog"

func init() {
	protolog.Register("protolog.testing.Foo", protolog.MessageType_MESSAGE_TYPE_EVENT, func() protolog.Message { return &Foo{} })
	protolog.Register("protolog.testing.Foo", protolog.MessageType_MESSAGE_TYPE_CONTEXT, func() protolog.Message { return &Foo{} })
	protolog.Register("protolog.testing.Bar", protolog.MessageType_MESSAGE_TYPE_EVENT, func() protolog.Message { return &Bar{} })
	protolog.Register("protolog.testing.Bar", protolog.MessageType_MESSAGE_TYPE_CONTEXT, func() protolog.Message { return &Bar{} })
	protolog.Register("protolog.testing.Baz", protolog.MessageType_MESSAGE_TYPE_EVENT, func() protolog.Message { return &Baz{} })
	protolog.Register("protolog.testing.Baz", protolog.MessageType_MESSAGE_TYPE_CONTEXT, func() protolog.Message { return &Baz{} })
	protolog.Register("protolog.testing.Baz.Bat", protolog.MessageType_MESSAGE_TYPE_EVENT, func() protolog.Message { return &Baz_Bat{} })
	protolog.Register("protolog.testing.Baz.Bat", protolog.MessageType_MESSAGE_TYPE_CONTEXT, func() protolog.Message { return &Baz_Bat{} })
	protolog.Register("protolog.testing.Baz.Bat.Ban", protolog.MessageType_MESSAGE_TYPE_EVENT, func() protolog.Message { return &Baz_Bat_Ban{} })
	protolog.Register("protolog.testing.Baz.Bat.Ban", protolog.MessageType_MESSAGE_TYPE_CONTEXT, func() protolog.Message { return &Baz_Bat_Ban{} })
	protolog.Register("protolog.testing.Empty", protolog.MessageType_MESSAGE_TYPE_EVENT, func() protolog.Message { return &Empty{} })
	protolog.Register("protolog.testing.Empty", protolog.MessageType_MESSAGE_TYPE_CONTEXT, func() protolog.Message { return &Empty{} })
}

func (m *Foo) ProtologName() string {
	return "protolog.testing.Foo"
}
func (m *Bar) ProtologName() string {
	return "protolog.testing.Bar"
}
func (m *Baz) ProtologName() string {
	return "protolog.testing.Baz"
}
func (m *Baz_Bat) ProtologName() string {
	return "protolog.testing.Baz.Bat"
}
func (m *Baz_Bat_Ban) ProtologName() string {
	return "protolog.testing.Baz.Bat.Ban"
}
func (m *Empty) ProtologName() string {
	return "protolog.testing.Empty"
}
