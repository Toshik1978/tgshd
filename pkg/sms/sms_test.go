package sms

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

func TestSMS(t *testing.T) {
	suite.Run(t, new(gammuTestSuite))
}
