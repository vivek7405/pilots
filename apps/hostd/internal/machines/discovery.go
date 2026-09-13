package machines

// Discoveries is several Discovery implementations as one.
//
// A machine's namespace now carries two listeners: the `.internal` responder
// and the credential broker. The lifecycle should not grow a second field and a
// second pair of nil checks for that -- and a third listener next year would
// grow a third. One fan-out, and the manager keeps knowing about exactly one
// thing that follows a namespace.
type Discoveries []Discovery

// Bind starts every member.
//
// A failure stops there and RELEASES the ones already bound, because a machine
// whose namespace holds half its listeners is worse than one that failed to
// start: the first is a machine that runs and is quietly missing a capability,
// and the second is an error somebody sees.
func (d Discoveries) Bind(machineID, netnsName string) error {
	for i, member := range d {
		if err := member.Bind(machineID, netnsName); err != nil {
			for _, bound := range d[:i] {
				bound.Release(machineID)
			}
			return err
		}
	}
	return nil
}

// Release stops every member, and keeps going past a member that is not bound.
func (d Discoveries) Release(machineID string) {
	for _, member := range d {
		member.Release(machineID)
	}
}
