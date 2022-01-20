package gammu

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

func TestGammu(t *testing.T) {
	suite.Run(t, new(builderTestSuite))
}
