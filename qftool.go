// Program qftool can read and write the SPI rom of the QuickFeather
// development board.
//
// Caution: this tool can brick your QuickFeather board, so the tool
// comes without warranty. See:
//
//   https://forum.quicklogic.com/viewtopic.php?t=29
//
// for ways to recover your board. It probably requires a J-Link tool
// from SEGGER.
//
// The image layout of the 2MiB of SPI ROM is:
//
//   0x00000-0x0ffff bootloader (metadata: 0x1f000)
//   0x20000-0x3ffff usb FPGA (metadata: 0x10000)
//   0x40000-0x5ffff app FPGA (metadata: 0x11000)
//   0x60000-0x7ffff app FFE (metadata: 0x12000)
//   0x80000-0xedfff app M4 code (metadata: 0x13000)
//
// The metadata captures info like the fact the corresponding section
// of the flash is occupied and its CRC value. The bootloader
// validates this CRC before loading and executing a section's
// content. Errors here tend to cause the bootloader to set the "red"
// LED to turn on.
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"sync"
	"time"

	"github.com/pkg/term"
	"zappem.net/pub/debug/xcrc32"
	"zappem.net/pub/debug/xxd"
)

const romSize = 2 * 1024 * 1024

type Present uint8

const (
	PresentWritten Present = 0x03
	PresentEmpty           = 0xff
)

func (p Present) String() string {
	switch p {
	case PresentWritten:
		return "written"
	case PresentEmpty:
		return "empty"
	default:
		return "<error>"
	}
}

type Type uint8

const (
	TypeM4   Type = 1
	TypeFFE       = 2
	TypeFPGA      = 3
	TypeFS        = 4
)

func (t Type) String() string {
	switch t {
	case TypeM4:
		return "m4"
	case TypeFFE:
		return "ffe"
	case TypeFPGA:
		return "fpga"
	case TypeFS:
		return "fs"
	default:
		return "<error>"
	}
}

type SubType uint8

const (
	SubTypeBoot  SubType = 1
	SubTypeApp           = 2
	SubTypeOTA           = 3
	SubTypeFSFat         = 0x20
)

func (t SubType) String() string {
	switch t {
	case SubTypeBoot:
		return "boot"
	case SubTypeApp:
		return "app"
	case SubTypeOTA:
		return "ota"
	case SubTypeFSFat:
		return "fs-FAT"
	default:
		return "<error>"
	}
}

// MetaData is the format of the meta data associated with each section.
type MetaData struct {
	// CRC is a composable CRC32 value whose computation is common
	// to the remote protocol for gdb and appears to have its
	// origins in libiberty/crc32.c
	CRC uint32

	// Size is the number of bytes written to the flash for the
	// section described by this metadata.
	Size uint32

	// Present is a single byte capturing the presence of the desired
	// object.
	Written Present

	// Type indicates the encoding.
	Image Type

	// SubType captures the role of the image in this section.
	Purpose SubType

	// Reserved holds 0xff
	Reserved uint8
}

type Section struct {
	name              string
	base, limit, meta int
	written           Present
	image             Type
	purpose           SubType
}

// sections holds the layout map for the flash.
var sections = []Section{
	{
		name:    "bootloader",
		base:    0x00000,
		limit:   0x10000,
		meta:    0x1f000,
		image:   TypeM4,
		purpose: SubTypeBoot,
	},
	{
		name:    "bootfpga",
		base:    0x20000,
		limit:   0x40000,
		meta:    0x10000,
		image:   TypeFPGA,
		purpose: SubTypeBoot,
	},
	{
		name:    "appfpga",
		base:    0x40000,
		limit:   0x60000,
		meta:    0x11000,
		image:   TypeFPGA,
		purpose: SubTypeApp,
	},
	{
		name:    "appffe",
		base:    0x60000,
		limit:   0x80000,
		meta:    0x12000,
		image:   TypeFFE,
		purpose: SubTypeApp,
	},
	{
		name:    "app",
		base:    0x80000,
		limit:   0xee000,
		meta:    0x13000,
		image:   TypeM4,
		purpose: SubTypeApp,
	},
}

var (
	tty      = flag.String("tty", "/dev/serial/by-id/usb-1d50_6140-if00", "tty with which to connect to QuickFeather")
	latency  = flag.Duration("latency", 1*time.Second, "time to wait for desired status")
	rd       = flag.String("read", "", "read data from --section (or --addr to --limit) to target")
	wr       = flag.String("write", "", "write data to --section (or --addr up to --limit, without --meta)")
	protect  = flag.Int("protect", 0x40000, "base address below which writes are invalid")
	skip     = flag.Int("skip", 0, "bytes at start of file to skip when writing")
	debug    = flag.Bool("debug", false, "be more verbose")
	progress = flag.Bool("progress", true, "show progress")
	reset    = flag.Bool("reset", false, "reset device at startup")
	layout   = flag.Bool("layout", false, "list the layout of the flash and exit")
	check    = flag.Bool("check", false, "validate the CRC of the named section (see --layout)")
	disable  = flag.Bool("disable", false, "disable a section by overwriting its metadata")
	sect     = flag.String("section", "", "the section to operate on (overrides --addr, --limit and --meta)")
	addrArg  = flag.Int("addr", 0x80000, "base address for actual read or write (overridden by --section)")
	limitArg = flag.Int("limit", romSize, "one more than last address for read or writes (overridden by --section)")
)

// type QF holds an open connection to a QuickFeather USB serial port.
type QF struct {
	t      *term.Term
	mu     sync.Mutex
	reader *bufio.Reader
}

// Close closes down the motor control.
func (a *QF) Close() error {
	return a.t.Close()
}

// spi performs an SPI command using the TinyFPGA bootloader protocol.
func (a *QF) spi(cmds []byte, expect uint) ([]byte, error) {
	if expect > 16 || len(cmds) > 16 {
		return nil, fmt.Errorf("protocol limited to 16 byte payloads")
	}
	req := make([]byte, 5)
	resp := make([]byte, expect)
	req[0] = 1
	send := uint(len(cmds))
	req[1] = byte(send & 0xff)
	req[2] = byte((send >> 8) & 0xff)
	req[3] = byte(expect & 0xff)
	req[4] = byte((expect >> 8) & 0xff)
	if n, err := a.t.Write(append(req, cmds...)); err != nil {
		return nil, fmt.Errorf("failed to write enough [%d != %d]: %v", n, len(cmds)+len(req), err)
	}
	consumed := 0
	for uint(consumed) != expect {
		n, err := a.t.Read(resp[consumed:expect])
		if n == 0 && err != nil {
			return nil, fmt.Errorf("failed to read IDs [just %d bytes]: %v", consumed+n, err)
		}
		consumed += n
	}
	return resp, nil
}

var ErrTimedOut = errors.New("timed out")

// await polls the status register until masked by mask, it equals desired.
func (a *QF) await(mask, desired byte, timeout time.Duration) error {
	if timeout == 0 {
		timeout = *latency
	}
	until := time.After(timeout)
	for {
		if b, err := a.spi([]byte{0x05}, 1); err != nil {
			return fmt.Errorf("failed to read status: %v", err)
		} else if b[0]&mask == desired {
			return nil
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-until:
			return ErrTimedOut
		}
	}
}

const flashWIP = 1 << 0

var ErrWriteEnableFailed = errors.New("write enabled failed")

func (a *QF) writeEnable() error {
	if _, err := a.spi([]byte{0x06}, 0); err != nil {
		return ErrWriteEnableFailed
	}
	return nil
}

// Read reads n bytes from a specific address returning a byte array.
func (a *QF) Read(address, n int, ticker bool) ([]byte, error) {
	var result []byte
	tics := n / 50
	sofar := 0
	if ticker {
		fmt.Printf("read [0x%06x,0x%06x] ", address, address+n-1)
	}
	if address < 0 || address+n > romSize {
		return nil, fmt.Errorf("data read request outside [0x%x,0x%x)", 0, romSize)
	}
	for n > 0 {
		if sofar >= tics {
			if ticker {
				fmt.Print(".")
				sofar = 0
			}
		}
		cmd := make([]byte, 5)
		cmd[0] = 0x0B
		cmd[1] = byte((address >> 16) & 0xFF)
		cmd[2] = byte((address >> 8) & 0xFF)
		cmd[3] = byte(address & 0xFF)
		offset := address & 15
		delta := 16 - offset
		if delta > n {
			delta = n
		}
		d, err := a.spi(cmd, uint(delta))
		if err != nil {
			return nil, err
		}
		address += delta
		n -= delta
		sofar += delta
		result = append(result, d...)
	}
	if ticker {
		fmt.Println(" done")
	}
	return result, nil
}

// Write writes data array to address.
func (a *QF) Write(address int, data []byte, ticker bool) error {
	if address&0xfff != 0 {
		return fmt.Errorf("address is not sector aligned: 0x%06x & 0xfff != 0", address)
	}
	if len(data)&0xfff != 0 {
		extend := 0x1000 - (len(data) & 0xfff)
		actual := make([]byte, len(data)+extend)
		for i := 0; i < extend; i++ {
			actual[len(data)+i] = 0xff
		}
		copy(actual, data)
		data = actual
	}
	offset := 0
	n := len(data)
	tics := n / 50
	sofar := 0
	if ticker {
		fmt.Printf("write [0x%06x,0x%06x] ", address, address+n-1)
	}
	if address < 0 || address+n > romSize {
		return fmt.Errorf("data write request outside [0x%x,0x%x)", 0, romSize)
	}
	for n > 0 {
		if sofar >= tics {
			if ticker {
				fmt.Print(".")
				sofar = 0
			}
		}
		if address&0xfff == 0 {
			if ticker {
				fmt.Print("*\b")
			}
			cmd := make([]byte, 4)
			cmd[0] = 0x20
			cmd[1] = byte((address >> 16) & 0xFF)
			cmd[2] = byte((address >> 8) & 0xFF)
			cmd[3] = byte(address & 0xFF)
			if err := a.writeEnable(); err != nil {
				return fmt.Errorf("sector erase at address=0x%06x: %v", address, err)
			}
			if _, err := a.spi(cmd, 0); err != nil {
				return fmt.Errorf("sector erase failed for address=0x%06x: %v", address, err)
			}
			if err := a.await(flashWIP, 0, *latency); err != nil {
				return fmt.Errorf("sector erase error: %v", err)
			}
		}
		delta := n
		if delta > 8 {
			delta = 8
		}
		cmd := make([]byte, 4)
		cmd[0] = 0x02
		cmd[1] = byte((address >> 16) & 0xFF)
		cmd[2] = byte((address >> 8) & 0xFF)
		cmd[3] = byte(address & 0xFF)
		if err := a.writeEnable(); err != nil {
			return fmt.Errorf("write enable for programming failed address=0x%06x", address)
		}
		if _, err := a.spi(append(cmd, data[offset:offset+delta]...), 0); err != nil {
			return err
		}
		if err := a.await(flashWIP, 0, *latency); err != nil {
			return fmt.Errorf("sector erase error: %v", err)
		}
		address += delta
		offset += delta
		n -= delta
		sofar += delta
	}
	if ticker {
		fmt.Println(" done")
	}
	return nil
}

// reset waits to confirm that there is no output from the device
// and then tries to issue a wake the SPI ROM command.
func (a *QF) reset() error {
	if *reset {
		if _, err := a.spi([]byte{0x66}, 0); err != nil {
			return fmt.Errorf("failed to enable reset", err)
		}
		if _, err := a.spi([]byte{0x99}, 0); err != nil {
			return fmt.Errorf("failed to reset device", err)
		}
	}

	// Awake the ROM.
	if _, err := a.spi([]byte{0xAB}, 1); err != nil {
		return err
	}
	// Read device information
	b, err := a.spi([]byte{0x9F}, 3)
	if err != nil {
		return err
	}

	if b[0] != 0xC8 {
		return fmt.Errorf("got MID=0x%02X expected MID=0xC8", b[0])
	}
	if b[1] != 0x40 || b[2] != 0x15 {
		return fmt.Errorf("got DID=0x%02X,0x%02X expect 0x40,0x15", b[1], b[2])
	}

	if *debug {
		fmt.Printf("QuickFeather: MID=0x%02X, DID=0x%02X,0x%02X\n", b[0], b[1], b[2])
	}
	if err := a.await(0, 0, time.Second); err != nil {
		return fmt.Errorf("failed to read status: %v", err)
	}
	if _, err := a.Read(0, 16, false); err != nil {
		fmt.Println("failed to read first 16 bytes:", err)
	}

	return nil
}

// NewQF opens a connection to a QuickFeather via the specified tty
// device file.
func NewQF(tty string) (*QF, error) {
	t, err := term.Open(tty, term.Speed(115200), term.RawMode)
	if err != nil {
		return nil, fmt.Errorf("unable to open serial port: %v", err)
	}
	a := &QF{
		t:      t,
		reader: bufio.NewReader(t),
	}
	if err := a.reset(); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

// readMeta reads the meta data and decodes it from the specified address.
func (a *QF) readMeta(sec Section) (decoded MetaData) {
	m, err := a.Read(sec.meta, binary.Size(decoded), false)
	if err != nil {
		log.Fatalf("failed to read 16 bytes of metda data from sector %q: %v", sec.name, err)
	}
	if err := binary.Read(bytes.NewReader(m), binary.LittleEndian, &decoded); err != nil {
		log.Fatalf("failed to decode metadata for %q: %v", sec.name, err)
	}
	return
}

// displayLayout logs the layout of the flash.
func (a *QF) displayLayout() {
	log.Print("section    size/bytes [   base,  limit)    meta={   present     format    purpose}   xcrc32")
	log.Print("---------- ---------- ----------------- --------    -------    -------    -------  --------")
	for i := range sections {
		sec := sections[i]
		decoded := a.readMeta(sec)
		log.Printf("%-10s %10d [0x%05x,0x%05x) 0x%05x={%10v %10v %10v} %08X\n", sec.name, decoded.Size, sec.base, sec.limit, sec.meta, decoded.Written, decoded.Image, decoded.Purpose, decoded.CRC)
	}
}

// writeMeta writes the metadata for a section to the
// flash. Protection checking should be performed prior to calling
// this function.
func (a *QF) writeMeta(sec Section, m MetaData) {
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.LittleEndian, m); err != nil {
		log.Fatalf("failed to format metadata for %q: %v", sec.name, err)
	}
	if len(buf.Bytes()) != 12 {
		log.Fatalf("programming error with metadata for %q: %d and not 12 bytes", sec.name, len(buf.Bytes()))
	}
	if err := a.Write(sec.meta, buf.Bytes(), *progress); err != nil {
		log.Fatalf("failed to write metadata for %q: %v", sec.name, err)
	}
}

// secByName returns the section information for the named section.
func secByName(name string) Section {
	for i := range sections {
		sec := sections[i]
		if sec.name == name {
			return sec
		}
	}
	log.Fatalf("no section named %q: try --layout", name)
	return Section{}
}

// validate attempts to confirm the CRC of the named section
func (a *QF) validate(name string) error {
	sec := secByName(name)
	meta := a.readMeta(sec)
	if max := sec.limit - sec.base; meta.Size > uint32(max) {
		return fmt.Errorf("meta for %q has invalid size %d > %d", sec.name, meta.Size, max)
	}
	d, err := a.Read(sec.base, int(meta.Size), true)
	if err != nil {
		return fmt.Errorf("failed to read %q (size %b bytes): %v", sec.name, meta.Size, err)
	}
	_, crc := xcrc32.NewCRC32(d)
	if crc == meta.CRC {
		return nil
	}
	return fmt.Errorf("crc mismatch for %q: got=0x%08x want=0x%08x", sec.name, crc, meta.CRC)
}

func main() {
	flag.Parse()

	qf, err := NewQF(*tty)
	if err != nil {
		log.Fatalf("not a QuickFeather in programming mode [%q]: %v\n", *tty, err)
	}
	defer qf.Close()

	if *layout {
		qf.displayLayout()
		return
	}
	if *check {
		if *sect == "" {
			log.Fatal("--check requires a valid --section (hint: try --layout)")
		}
		err := qf.validate(*sect)
		if err == nil {
			log.Printf("%q OK", *sect)
			return
		}
		log.Fatalf("check of %q failed: %v", *sect, err)
	}

	addr := *addrArg
	limit := *limitArg
	meta := 0
	if *sect != "" {
		sec := secByName(*sect)
		addr = sec.base
		limit = sec.limit
		meta = sec.meta
		if *disable {
			// Disable a section by reseting only the metadata for it.
			if *protect > addr {
				log.Fatal("operation refers to --protect'ed section: aborting")
			}
			qf.writeMeta(sec, MetaData{
				CRC:      ^uint32(0),
				Size:     ^uint32(0),
				Written:  PresentEmpty,
				Image:    sec.image,
				Purpose:  sec.purpose,
				Reserved: 0xff,
			})
			return
		}
	}
	if addr >= limit {
		log.Fatalf("require address=0x%x to be less than limit=0x%0x", addr, limit)
	}
	if limit > romSize {
		log.Fatalf("limit (0x%x) must not be above romSize=0x%x", limit, romSize)
	}

	if *rd != "" {
		if *sect != "" {
			sec := secByName(*sect)
			m := qf.readMeta(sec)
			if sLimit := addr + int(m.Size); sLimit < limit {
				limit = sLimit
			}
		}
		d, err := qf.Read(addr, limit-addr, *progress)
		if err != nil {
			log.Fatal("failed to read:", err)
		}
		if *rd != "-" {
			if err := ioutil.WriteFile(*rd, d, 0644); err != nil {
				log.Fatal("failed to read SPI ROM to file:", err)
			}
		} else {
			xxd.Print(addr, d)
		}
		os.Exit(0)
	}

	if *wr != "" {
		d, err := ioutil.ReadFile(*wr)
		if err != nil {
			log.Fatalf("failed to read %q: %v", *wr, err)
		}
		if *skip > len(d) {
			log.Fatalf("unable to skip=0x%06x bytes of 0x%06x from file %q", *skip, len(d), *wr)
		}
		if *skip != 0 {
			d = d[*skip:]
		}
		if len(d)+addr > limit {
			log.Fatalf("data from %q would write beyond limit (0x%06x) %d bytes too long", *wr, limit, len(d)-(limit-addr))
		}
		if addr < *protect {
			log.Fatalf("write protection up to 0x%06x, but address=0x%06x", *protect, addr)
		}
		if *debug {
			fmt.Printf("writing %q data from offset 0x%06x (0x%06x bytes) to 0x%06x...\n", *wr, *skip, len(d), addr)
		}
		if err := qf.Write(addr, d, *progress); err != nil {
			log.Fatalf("failed to write data: %v", err)
		}

		// Note, this overrides the --protect limit to update the metadata.
		if meta != 0 {
			sec := secByName(*sect)
			_, crc := xcrc32.NewCRC32(d)
			qf.writeMeta(sec, MetaData{
				CRC:      crc,
				Size:     uint32(len(d)),
				Written:  PresentWritten,
				Image:    sec.image,
				Purpose:  sec.purpose,
				Reserved: 0xff,
			})
		}
	}
}
