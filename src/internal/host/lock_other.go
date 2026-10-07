//go:build !linux

package host

import "io"

func (s *Store) Acquire() (io.Closer, error) { return nil, ErrUnsupportedHost }
