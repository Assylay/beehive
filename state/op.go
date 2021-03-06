package state

// OpType is the type of an operation in a transaction.
type OpType int

// Valid values for OpType.
const (
	Unknown OpType = iota
	Put            = iota
	Del            = iota
)

// Op is a state operation in a transaction.
type Op struct {
	T OpType
	D string // Dictionary.
	K string // Key.
	V []byte // Value.
}
