package postgres

import "time"

// timestamp existe só para dar um endereço estável ao Scan de time.Time.
type timestamp struct{ t time.Time }
