package redwood

import (
	"github.com/pkg/errors"
)

var (
	Err403 = errors.New("403: nope")
	Err404 = errors.New("not found")
)
