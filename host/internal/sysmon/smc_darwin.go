package sysmon

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The System Management Controller holds the fans' speeds. Its user
// client takes one struct in and gives one back (after smcFanControl and
// gopsutil); no privileges are needed to read.
const (
	smcHandleEvent = 2 // the struct method
	smcReadKey     = 5
	smcGetKeyInfo  = 9
)

// smcParam is SMCParamStruct: 80 bytes, laid out as the C struct.
type smcParam struct {
	key  uint32
	vers struct {
		major, minor, build, reserved byte
		release                       uint16
	}
	pLimit struct {
		version, length                uint16
		cpuPLimit, gpuPLimit, memLimit uint32
	}
	keyInfo struct {
		dataSize, dataType uint32
		attributes         byte
	}
	result, status, data8 byte
	data32                uint32
	bytes                 [32]byte
}

var iokit struct {
	once sync.Once
	err  error

	matching   func(name string) uintptr
	service    func(mainPort uint32, matching uintptr) uint32
	open       func(service, task, typ uint32, conn *uint32) int32
	closeConn  func(conn uint32) int32
	release    func(object uint32) int32
	callStruct func(conn, selector uint32, in unsafe.Pointer, inSize uintptr, out unsafe.Pointer, outSize *uintptr) int32
	taskSelf   func() uint32
}

// loadIOKit binds the IOKit calls once. Library handles stay open for the
// process: closing them under the Go runtime crashes (gopsutil #1832).
func loadIOKit() error {
	iokit.once.Do(func() {
		kit, err := purego.Dlopen("/System/Library/Frameworks/IOKit.framework/IOKit", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
		if err != nil {
			iokit.err = err
			return
		}
		sys, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
		if err != nil {
			iokit.err = err
			return
		}
		purego.RegisterLibFunc(&iokit.matching, kit, "IOServiceMatching")
		purego.RegisterLibFunc(&iokit.service, kit, "IOServiceGetMatchingService")
		purego.RegisterLibFunc(&iokit.open, kit, "IOServiceOpen")
		purego.RegisterLibFunc(&iokit.closeConn, kit, "IOServiceClose")
		purego.RegisterLibFunc(&iokit.release, kit, "IOObjectRelease")
		purego.RegisterLibFunc(&iokit.callStruct, kit, "IOConnectCallStructMethod")
		purego.RegisterLibFunc(&iokit.taskSelf, sys, "mach_task_self")
	})
	return iokit.err
}

type smc struct {
	conn uint32
}

func openSMC() (*smc, error) {
	if err := loadIOKit(); err != nil {
		return nil, err
	}
	svc := iokit.service(0, iokit.matching("AppleSMC")) // takes the matching dictionary
	if svc == 0 {
		return nil, errors.New("AppleSMC not found")
	}
	defer iokit.release(svc)
	var conn uint32
	if r := iokit.open(svc, iokit.taskSelf(), 0, &conn); r != 0 {
		return nil, fmt.Errorf("IOServiceOpen: %#x", r)
	}
	return &smc{conn: conn}, nil
}

func (s *smc) close() { iokit.closeConn(s.conn) }

func (s *smc) call(in *smcParam) (*smcParam, error) {
	out := new(smcParam)
	size := unsafe.Sizeof(*out)
	if r := iokit.callStruct(s.conn, smcHandleEvent, unsafe.Pointer(in), unsafe.Sizeof(*in), unsafe.Pointer(out), &size); r != 0 {
		return nil, fmt.Errorf("IOConnectCallStructMethod: %#x", r)
	}
	if out.result != 0 {
		return nil, fmt.Errorf("smc result %d", out.result)
	}
	return out, nil
}

// read returns a key's value as a number.
func (s *smc) read(key string) (float64, error) {
	in := &smcParam{key: binary.BigEndian.Uint32([]byte(key)), data8: smcGetKeyInfo}
	info, err := s.call(in)
	if err != nil {
		return 0, err
	}
	in.keyInfo.dataSize = info.keyInfo.dataSize
	in.data8 = smcReadKey
	out, err := s.call(in)
	if err != nil {
		return 0, err
	}
	n := min(int(info.keyInfo.dataSize), len(out.bytes))
	return smcNumber(info.keyInfo.dataType, out.bytes[:n])
}

// fans reads every fan's speed and maximum.
func (s *smc) fans() ([]Fan, error) {
	n, err := s.read("FNum")
	if err != nil {
		return nil, err
	}
	var fans []Fan
	for i := range min(int(n), 8) {
		rpm, err := s.read(fmt.Sprintf("F%dAc", i))
		if err != nil {
			return nil, err
		}
		top, _ := s.read(fmt.Sprintf("F%dMx", i))
		fans = append(fans, Fan{RPM: max(rpm, 0), Max: max(top, 0)})
	}
	return fans, nil
}
