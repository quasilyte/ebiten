// Copyright 2022 The Ebiten Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !android && !nintendosdk
// +build !android,!nintendosdk

package gamepad

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"github.com/hajimehoshi/ebiten/v2/internal/gamepaddb"
)

func byteSliceToString(s []byte) string {
	if i := bytes.IndexByte(s, 0); i != -1 {
		s = s[:i]
	}
	return string(s)
}

const dirName = "/dev/input"

var reEvent = regexp.MustCompile(`^event[0-9]+$`)

func isBitSet(s []byte, bit int) bool {
	return s[bit/8]&(1<<(bit%8)) != 0
}

type nativeGamepadsImpl struct {
	inotify int
	watch   int
}

func newNativeGamepadsImpl() nativeGamepads {
	return &nativeGamepadsImpl{}
}

func (g *nativeGamepadsImpl) init(gamepads *gamepads) error {
	// Check the existence of the directory `dirName`.
	var stat syscall.Stat_t
	if err := syscall.Stat(dirName, &stat); err != nil {
		if err == syscall.ENOENT {
			return nil
		}
		return fmt.Errorf("gamepad: Stat failed: %w", err)
	}
	if stat.Mode&syscall.S_IFDIR == 0 {
		return nil
	}

	inotify, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		return fmt.Errorf("gamepad: InotifyInit1 failed: %w", err)
	}
	g.inotify = inotify

	if g.inotify > 0 {
		// Register for IN_ATTRIB to get notified when udev is done.
		// This works well in practice but the true way is libudev.
		watch, err := syscall.InotifyAddWatch(g.inotify, dirName, syscall.IN_CREATE|syscall.IN_ATTRIB|syscall.IN_DELETE)
		if err != nil {
			return fmt.Errorf("gamepad: InotifyAddWatch failed: %w", err)
		}
		g.watch = watch
	}

	ents, err := os.ReadDir(dirName)
	if err != nil {
		return fmt.Errorf("gamepad: ReadDir(%s) failed: %w", dirName, err)
	}
	for _, ent := range ents {
		if ent.IsDir() {
			continue
		}
		if !reEvent.MatchString(ent.Name()) {
			continue
		}
		if err := g.openGamepad(gamepads, filepath.Join(dirName, ent.Name())); err != nil {
			return err
		}
	}

	return nil
}

func (*nativeGamepadsImpl) openGamepad(gamepads *gamepads, path string) (err error) {
	if gamepads.find(func(gamepad *Gamepad) bool {
		return gamepad.native.(*nativeGamepadImpl).path == path
	}) != nil {
		return nil
	}

	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if err == syscall.EACCES {
			return nil
		}
		// This happens with the Snap sandbox.
		if err == syscall.EPERM {
			return nil
		}
		// This happens just after a disconnection.
		if err == syscall.ENOENT {
			return nil
		}
		return fmt.Errorf("gamepad: Open failed: %w", err)
	}
	defer func() {
		if err != nil {
			_ = syscall.Close(fd)
		}
	}()

	evBits := make([]byte, (_EV_CNT+7)/8)
	keyBits := make([]byte, (_KEY_CNT+7)/8)
	absBits := make([]byte, (_ABS_CNT+7)/8)
	var id input_id
	if err := ioctl(fd, _EVIOCGBIT(0, uint(len(evBits))), unsafe.Pointer(&evBits[0])); err != nil {
		return fmt.Errorf("gamepad: ioctl for evBits failed: %w", err)
	}
	if err := ioctl(fd, _EVIOCGBIT(_EV_KEY, uint(len(keyBits))), unsafe.Pointer(&keyBits[0])); err != nil {
		return fmt.Errorf("gamepad: ioctl for keyBits failed: %w", err)
	}
	if err := ioctl(fd, _EVIOCGBIT(_EV_ABS, uint(len(absBits))), unsafe.Pointer(&absBits[0])); err != nil {
		return fmt.Errorf("gamepad: ioctl for absBits failed: %w", err)
	}
	if err := ioctl(fd, _EVIOCGID(), unsafe.Pointer(&id)); err != nil {
		return fmt.Errorf("gamepad: ioctl for an ID failed: %w", err)
	}

	if !isBitSet(evBits, _EV_KEY) {
		if err := syscall.Close(fd); err != nil {
			return err
		}

		return nil
	}
	if !isBitSet(evBits, _EV_ABS) {
		if err := syscall.Close(fd); err != nil {
			return err
		}

		return nil
	}

	cname := make([]byte, 256)
	name := "Unknown"
	// TODO: Is it OK to ignore the error here?
	if err := ioctl(fd, uint(_EVIOCGNAME(uint(len(cname)))), unsafe.Pointer(&cname[0])); err == nil {
		name = byteSliceToString(cname)
	}

	var sdlID string
	if id.vendor != 0 && id.product != 0 && id.version != 0 {
		sdlID = fmt.Sprintf("%02x%02x0000%02x%02x0000%02x%02x0000%02x%02x0000",
			byte(id.bustype), byte(id.bustype>>8),
			byte(id.vendor), byte(id.vendor>>8),
			byte(id.product), byte(id.product>>8),
			byte(id.version), byte(id.version>>8))
	} else {
		bs := []byte(name)
		if len(bs) < 12 {
			bs = append(bs, make([]byte, 12-len(bs))...)
		}
		sdlID = fmt.Sprintf("%02x%02x0000%02x%02x%02x%02x%02x%02x%02x%02x%02x%02x%02x%02x",
			byte(id.bustype), byte(id.bustype>>8),
			bs[0], bs[1], bs[2], bs[3], bs[4], bs[5], bs[6], bs[7], bs[8], bs[9], bs[10], bs[11])
	}

	n := &nativeGamepadImpl{
		path: path,
		fd:   fd,
	}
	gp := gamepads.add(name, sdlID)
	gp.native = n
	runtime.SetFinalizer(gp, func(gp *Gamepad) {
		n.close()
	})

	var axisCount int
	var buttonCount int
	var hatCount int
	for code := _BTN_MISC; code < _KEY_CNT; code++ {
		if !isBitSet(keyBits, code) {
			continue
		}
		n.keyMap[code-_BTN_MISC] = buttonCount
		buttonCount++
	}
	for code := 0; code < _ABS_CNT; code++ {
		n.absMap[code] = -1
		if !isBitSet(absBits, code) {
			continue
		}
		if code >= _ABS_HAT0X && code <= _ABS_HAT3Y {
			n.absMap[code] = hatCount
			hatCount++
			// Skip Y.
			code++
			continue
		}
		if err := ioctl(n.fd, uint(_EVIOCGABS(uint(code))), unsafe.Pointer(&n.absInfo[code])); err != nil {
			return fmt.Errorf("gamepad: ioctl for an abs at openGamepad failed: %w", err)
		}
		n.absMap[code] = axisCount
		axisCount++
	}

	n.axisCount_ = axisCount
	n.buttonCount_ = buttonCount
	n.hatCount_ = hatCount

	if err := n.pollAbsState(); err != nil {
		return err
	}

	return nil
}

func (g *nativeGamepadsImpl) update(gamepads *gamepads) error {
	if g.inotify <= 0 {
		return nil
	}

	buf := make([]byte, 16384)
	n, err := syscall.Read(g.inotify, buf[:])
	if err != nil {
		if err == syscall.EAGAIN {
			return nil
		}
		return fmt.Errorf("gamepad: Read failed: %w", err)
	}
	buf = buf[:n]

	for len(buf) > 0 {
		e := syscall.InotifyEvent{
			Wd:     int32(buf[0]) | int32(buf[1])<<8 | int32(buf[2])<<16 | int32(buf[3])<<24,
			Mask:   uint32(buf[4]) | uint32(buf[5])<<8 | uint32(buf[6])<<16 | uint32(buf[7])<<24,
			Cookie: uint32(buf[8]) | uint32(buf[9])<<8 | uint32(buf[10])<<16 | uint32(buf[11])<<24,
			Len:    uint32(buf[12]) | uint32(buf[13])<<8 | uint32(buf[14])<<16 | uint32(buf[15])<<24,
		}
		name := byteSliceToString(buf[16 : 16+e.Len-1]) // len includes the null termiinate.
		buf = buf[16+e.Len:]
		if !reEvent.MatchString(name) {
			continue
		}

		path := filepath.Join(dirName, name)
		if e.Mask&(syscall.IN_CREATE|syscall.IN_ATTRIB) != 0 {
			if err := g.openGamepad(gamepads, path); err != nil {
				return err
			}
			continue
		}
		if e.Mask&syscall.IN_DELETE != 0 {
			if gp := gamepads.find(func(gamepad *Gamepad) bool {
				return gamepad.native.(*nativeGamepadImpl).path == path
			}); gp != nil {
				gp.native.(*nativeGamepadImpl).close()
				gamepads.remove(func(gamepad *Gamepad) bool {
					return gamepad == gp
				})
			}
			continue
		}
	}

	return nil
}

type nativeGamepadImpl struct {
	fd      int
	path    string
	keyMap  [_KEY_CNT - _BTN_MISC]int
	absMap  [_ABS_CNT]int
	absInfo [_ABS_CNT]input_absinfo
	dropped bool

	axes    [_ABS_CNT]float64
	buttons [_KEY_CNT - _BTN_MISC]bool
	hats    [4]int

	axisCount_   int
	buttonCount_ int
	hatCount_    int
}

func (g *nativeGamepadImpl) close() {
	if g.fd != 0 {
		_ = syscall.Close(g.fd)
	}
	g.fd = 0
}

func (g *nativeGamepadImpl) update(gamepad *gamepads) error {
	if g.fd == 0 {
		return nil
	}

	for {
		buf := make([]byte, unsafe.Sizeof(input_event{}))
		// TODO: Should the returned byte count be cared?
		if _, err := syscall.Read(g.fd, buf); err != nil {
			if err == syscall.EAGAIN {
				break
			}
			// Disconnected
			if err == syscall.ENODEV {
				g.close()
				return nil
			}
			return fmt.Errorf("gamepad: Read failed: %w", err)
		}

		const (
			offsetTyp   = unsafe.Offsetof(input_event{}.typ)
			offsetCode  = unsafe.Offsetof(input_event{}.code)
			offsetValue = unsafe.Offsetof(input_event{}.value)
		)
		// time is not used.
		e := input_event{
			typ:   uint16(buf[offsetTyp]) | uint16(buf[offsetTyp+1])<<8,
			code:  uint16(buf[offsetCode]) | uint16(buf[offsetCode+1])<<8,
			value: int32(buf[offsetValue]) | int32(buf[offsetValue+1])<<8 | int32(buf[offsetValue+2])<<16 | int32(buf[offsetValue+3])<<24,
		}

		if e.typ == _EV_SYN {
			switch e.code {
			case _SYN_DROPPED:
				g.dropped = true
			case _SYN_REPORT:
				g.dropped = false
				if err := g.pollAbsState(); err != nil {
					return fmt.Errorf("gamepad: poll absolute state: %w", err)
				}
			}
		}
		if g.dropped {
			continue
		}

		switch e.typ {
		case _EV_KEY:
			if int(e.code-_BTN_MISC) < len(g.keyMap) {
				idx := g.keyMap[e.code-_BTN_MISC]
				g.buttons[idx] = e.value != 0
			}
		case _EV_ABS:
			g.handleAbsEvent(int(e.code), e.value)
		}
	}
	return nil
}

func (g *nativeGamepadImpl) pollAbsState() error {
	for code := 0; code < _ABS_CNT; code++ {
		if g.absMap[code] < 0 {
			continue
		}
		if err := ioctl(g.fd, uint(_EVIOCGABS(uint(code))), unsafe.Pointer(&g.absInfo[code])); err != nil {
			return fmt.Errorf("gamepad: ioctl for an abs at pollAbsState failed: %w", err)
		}
		g.handleAbsEvent(code, g.absInfo[code].value)
	}
	return nil
}

func (g *nativeGamepadImpl) handleAbsEvent(code int, value int32) {
	index := g.absMap[code]

	if code >= _ABS_HAT0X && code <= _ABS_HAT3Y {
		axis := (code - _ABS_HAT0X) % 2

		switch axis {
		case 0:
			switch {
			case value < 0:
				g.hats[index] |= hatLeft
				g.hats[index] &^= hatRight
			case value > 0:
				g.hats[index] &^= hatLeft
				g.hats[index] |= hatRight
			default:
				g.hats[index] &^= hatLeft | hatRight
			}
		case 1:
			switch {
			case value < 0:
				g.hats[index] |= hatUp
				g.hats[index] &^= hatDown
			case value > 0:
				g.hats[index] &^= hatUp
				g.hats[index] |= hatDown
			default:
				g.hats[index] &^= hatUp | hatDown
			}
		}
		return
	}

	info := g.absInfo[code]
	v := float64(value)
	if r := float64(info.maximum) - float64(info.minimum); r != 0 {
		v = (v - float64(info.minimum)) / r
		v = v*2 - 1
	}
	g.axes[index] = v
}

func (*nativeGamepadImpl) hasOwnStandardLayoutMapping() bool {
	return false
}

func (*nativeGamepadImpl) isStandardAxisAvailableInOwnMapping(axis gamepaddb.StandardAxis) bool {
	return false
}

func (*nativeGamepadImpl) isStandardButtonAvailableInOwnMapping(button gamepaddb.StandardButton) bool {
	return false
}

func (g *nativeGamepadImpl) axisCount() int {
	return g.axisCount_
}

func (g *nativeGamepadImpl) buttonCount() int {
	return g.buttonCount_
}

func (g *nativeGamepadImpl) hatCount() int {
	return g.hatCount_
}

func (g *nativeGamepadImpl) axisValue(axis int) float64 {
	if axis < 0 || axis >= g.axisCount_ {
		return 0
	}
	return g.axes[axis]
}

func (g *nativeGamepadImpl) isButtonPressed(button int) bool {
	if button < 0 || button >= g.buttonCount_ {
		return false
	}
	return g.buttons[button]
}

func (*nativeGamepadImpl) buttonValue(button int) float64 {
	panic("gamepad: buttonValue is not implemented")
}

func (g *nativeGamepadImpl) hatState(hat int) int {
	if hat < 0 || hat >= g.hatCount_ {
		return hatCentered
	}
	return g.hats[hat]
}

func (g *nativeGamepadImpl) vibrate(duration time.Duration, strongMagnitude float64, weakMagnitude float64) {
	// TODO: Implement this (#1452)
}
