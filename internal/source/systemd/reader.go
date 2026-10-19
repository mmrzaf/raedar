package systemd

import (
	"context"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// The systemd D-Bus names.
const (
	busName             = "org.freedesktop.systemd1"
	managerPath         = dbus.ObjectPath("/org/freedesktop/systemd1")
	managerInterface    = "org.freedesktop.systemd1.Manager"
	unitInterface       = "org.freedesktop.systemd1.Unit"
	serviceInterface    = "org.freedesktop.systemd1.Service"
	timerInterface      = "org.freedesktop.systemd1.Timer"
	propertiesInterface = "org.freedesktop.DBus.Properties"
)

// unitStatus is one unit as ListUnits reports it.
type unitStatus struct {
	Name        string
	Description string
	LoadState   string
	ActiveState string
	SubState    string
	Followed    string
	Path        dbus.ObjectPath
	JobID       uint32
	JobType     string
	JobPath     dbus.ObjectPath
}

// Bus reads unit state from the system bus.
type Bus struct {
	conn  *dbus.Conn
	units map[string]dbus.ObjectPath
}

// connect opens the system bus socket at path.
// Connect opens the system D-Bus socket at path.
func Connect(socketPath string) (*Bus, error) {
	conn, err := dbus.Dial("unix:path=" + socketPath)
	if err != nil {
		return nil, err
	}
	if err := conn.Auth(nil); err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Hello(); err != nil {
		conn.Close()
		return nil, err
	}
	return &Bus{conn: conn}, nil
}

// Close closes the bus connection.
func (r *Bus) Close() {
	if r.conn != nil {
		r.conn.Close()
	}
}

// ListUnits returns the state of every unit, with the result and exit status
// of failed units and, for timers, of the unit the timer activates.
func (r *Bus) ListUnits(ctx context.Context) ([]Unit, error) {
	var listed []unitStatus
	manager := r.conn.Object(busName, managerPath)
	if err := manager.CallWithContext(ctx, managerInterface+".ListUnits", 0).Store(&listed); err != nil {
		return nil, err
	}
	r.units = make(map[string]dbus.ObjectPath, len(listed))
	for _, status := range listed {
		r.units[status.Name] = status.Path
	}
	units := make([]Unit, 0, len(listed))
	for _, status := range listed {
		unit := Unit{
			Name:        status.Name,
			ActiveState: status.ActiveState,
			SubState:    status.SubState,
		}
		if status.ActiveState == "failed" {
			if err := r.addResult(ctx, status.Path, &unit); err != nil {
				return nil, err
			}
		}
		units = append(units, unit)
	}
	for i := range units {
		if !isTimer(units[i].Name) {
			continue
		}
		activated, err := r.timerUnit(ctx, r.units[units[i].Name])
		if err != nil {
			return nil, err
		}
		if path, ok := r.units[activated]; ok {
			if err := r.addResult(ctx, path, &units[i]); err != nil {
				return nil, err
			}
		}
	}
	return units, nil
}

// addResult reads the result, exit status and state change time of a unit.
func (r *Bus) addResult(ctx context.Context, path dbus.ObjectPath, unit *Unit) error {
	if path == "" {
		return nil
	}
	object := r.conn.Object(busName, path)
	properties, err := r.properties(ctx, object, unitInterface)
	if err != nil {
		return err
	}
	if value, ok := properties["StateChangeTimestamp"]; ok {
		if usec, ok := value.Value().(uint64); ok && usec > 0 {
			at := time.Unix(0, int64(usec)*1000).UTC()
			unit.Since = &at
		}
	}
	if !isService(unit.Name) {
		return nil
	}
	properties, err = r.properties(ctx, object, serviceInterface)
	if err != nil {
		return err
	}
	if value, ok := properties["Result"]; ok {
		if result, ok := value.Value().(string); ok {
			unit.Result = result
		}
	}
	if value, ok := properties["ExecMainStatus"]; ok {
		if status, ok := value.Value().(int32); ok {
			exit := int(status)
			unit.ExitStatus = &exit
		}
	}
	return nil
}

// timerUnit returns the unit a timer activates.
func (r *Bus) timerUnit(ctx context.Context, path dbus.ObjectPath) (string, error) {
	if path == "" {
		return "", nil
	}
	properties, err := r.properties(ctx, r.conn.Object(busName, path), timerInterface)
	if err != nil {
		return "", err
	}
	if value, ok := properties["Unit"]; ok {
		if name, ok := value.Value().(string); ok {
			return name, nil
		}
	}
	return "", nil
}

// properties reads every property of one interface.
func (r *Bus) properties(ctx context.Context, object dbus.BusObject, iface string) (map[string]dbus.Variant, error) {
	var properties map[string]dbus.Variant
	if err := object.CallWithContext(ctx, propertiesInterface+".GetAll", 0, iface).Store(&properties); err != nil {
		return nil, err
	}
	return properties, nil
}

// isTimer reports whether a unit name is a timer.
func isTimer(name string) bool {
	return strings.HasSuffix(name, ".timer")
}

// isService reports whether a unit name is a service.
func isService(name string) bool {
	return strings.HasSuffix(name, ".service")
}
