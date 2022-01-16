package sms

import (
	"fmt"

	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type gammuTestSuite struct {
	suite.Suite

	ucs2Text  string
	ucs2Parts []string
}

func (s *gammuTestSuite) SetupSuite() {
	s.ucs2Text = `Очень длинная тестовая строка.
Очень длинная тестовая строка.
Очень длинная тестовая строка.
Очень длинная тестовая строка.
Очень длинная тестовая строка.
Очень длинная тестовая строка.`
	s.ucs2Parts = []string{
		`Очень длинная тестовая строка.
Очень длинная тестовая строка.
Очень`,
		` длинная тестовая строка.
Очень длинная тестовая строка.
Очень длин`,
		`ная тестовая строка.
Очень длинная тестовая строка.`,
	}
}

func (s *gammuTestSuite) TestBuild1() {
	zapCore, zapRecorded := observer.New(zap.InfoLevel)
	g := NewGammu(zap.New(zapCore), nil)

	parts := g.build("Test string")
	s.Len(parts, 1)
	s.Equal("", parts[0].UDH)
	s.Equal(defaultEncoding, parts[0].Coding)
	s.Equal("Test string", parts[0].Text)

	s.EqualValues(1, zapRecorded.Len())
}

func (s *gammuTestSuite) TestBuild2() {
	zapCore, zapRecorded := observer.New(zap.InfoLevel)
	g := NewGammu(zap.New(zapCore), nil)

	parts := g.build("Тестовая строка")
	s.Len(parts, 1)
	s.Equal("", parts[0].UDH)
	s.Equal(ucs2Encoding, parts[0].Coding)
	s.Equal("Тестовая строка", parts[0].Text)

	s.EqualValues(1, zapRecorded.Len())
}

func (s *gammuTestSuite) TestBuild3() {
	zapCore, zapRecorded := observer.New(zap.InfoLevel)
	g := NewGammu(zap.New(zapCore), nil)

	parts := g.build(s.ucs2Text)
	s.Len(parts, len(s.ucs2Parts))
	for i := range parts {
		s.Equal(ucs2Encoding, parts[i].Coding)
		s.Equal(s.ucs2Parts[i], parts[i].Text)
		s.Equal(fmt.Sprintf("%02X%02X", len(s.ucs2Parts), i+1), parts[i].UDH[8:])
	}

	s.EqualValues(1, zapRecorded.Len())
}

func (s *gammuTestSuite) TestUDH() {
	zapCore, zapRecorded := observer.New(zap.InfoLevel)
	g := NewGammu(zap.New(zapCore), nil)

	s.Equal("0500031A1001", g.buildUDH(0x1a, 0x10, 0x01))
	s.EqualValues(1, zapRecorded.Len())
}
