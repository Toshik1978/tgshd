package gammu

import (
	"context"
	"fmt"

	"github.com/stretchr/testify/suite"
)

type builderTestSuite struct {
	suite.Suite

	ucs2Text  string
	ucs2Parts []string
}

func (s *builderTestSuite) SetupSuite() {
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

func (s *builderTestSuite) TestBuild1() {
	builder := NewSequenceBuilder()
	parts := builder.Do(context.Background(), "Test string")

	s.Len(parts, 1)
	s.Equal("", parts[0].UDH)
	s.Equal(defaultEncoding, parts[0].Coding)
	s.Equal("Test string", parts[0].Text)
}

func (s *builderTestSuite) TestBuild2() {
	builder := NewSequenceBuilder()
	parts := builder.Do(context.Background(), "Тестовая строка")

	s.Len(parts, 1)
	s.Equal("", parts[0].UDH)
	s.Equal(ucs2Encoding, parts[0].Coding)
	s.Equal("Тестовая строка", parts[0].Text)
}

func (s *builderTestSuite) TestBuild3() {
	builder := NewSequenceBuilder()
	parts := builder.Do(context.Background(), s.ucs2Text)

	s.Len(parts, len(s.ucs2Parts))
	for i := range parts {
		s.Equal(ucs2Encoding, parts[i].Coding)
		s.Equal(s.ucs2Parts[i], parts[i].Text)
		s.Equal(fmt.Sprintf("%02X%02X", len(s.ucs2Parts), i+1), parts[i].UDH[8:])
	}
}

func (s *builderTestSuite) TestUDH() {
	builder := NewSequenceBuilder()
	s.Equal("0500031A1001", builder.buildUDH(0x1a, 0x10, 0x01))
}
