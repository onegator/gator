package a2a

// Module is both sides, for the server to hand out.
type Module struct {
	// Remote is the project's agents outside Gator and the calls that answer their turns.
	Remote *Remote
	// Inbound is the clients let in, the exposed agents' cards and the endpoint at /a2a.
	Inbound *Inbound
}
