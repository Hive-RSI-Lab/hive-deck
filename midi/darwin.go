//go:build darwin && cgo

package midi

/*
#cgo LDFLAGS: -framework CoreMIDI -framework CoreFoundation
#include <CoreMIDI/CoreMIDI.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>

extern void deckMidiRead(int id, unsigned char *data, int len);

static void deckReadProc(const MIDIPacketList *list, void *readRef, void *srcRef) {
	const MIDIPacket *p = &list->packet[0];
	for (UInt32 i = 0; i < list->numPackets; i++) {
		deckMidiRead((int)(intptr_t)readRef, (unsigned char *)p->data, (int)p->length);
		p = MIDIPacketNext(p);
	}
}

static int deckName(MIDIObjectRef obj, char *buf, int n) {
	CFStringRef s = NULL;
	if (MIDIObjectGetStringProperty(obj, kMIDIPropertyDisplayName, &s) != noErr || s == NULL) {
		return 0;
	}
	Boolean ok = CFStringGetCString(s, buf, n, kCFStringEncodingUTF8);
	CFRelease(s);
	return ok ? 1 : 0;
}

static OSStatus deckOpenIn(MIDIClientRef client, MIDIEndpointRef src, int id, MIDIPortRef *port) {
	OSStatus err = MIDIInputPortCreate(client, CFSTR("deck in"), deckReadProc, (void *)(intptr_t)id, port);
	if (err != noErr) {
		return err;
	}
	return MIDIPortConnectSource(*port, src, NULL);
}

static OSStatus deckSend(MIDIPortRef port, MIDIEndpointRef dst, unsigned char *data, int len) {
	Byte buf[64];
	MIDIPacketList *list = (MIDIPacketList *)buf;
	MIDIPacket *p = MIDIPacketListInit(list);
	p = MIDIPacketListAdd(list, sizeof(buf), p, 0, len, data);
	if (p == NULL) {
		return -1;
	}
	return MIDISend(port, dst, list);
}
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

// On macOS a USB MIDI device is a CoreMIDI source and destination. The read
// callback runs on CoreMIDI's thread and hands bytes to the port's parser.

var (
	clientOnce sync.Once
	client     C.MIDIClientRef
	clientErr  error

	portsMu sync.Mutex
	ports   = map[int]*macPort{}
	nextID  = 1
)

func midiClient() (C.MIDIClientRef, error) {
	clientOnce.Do(func() {
		name := C.CFStringCreateWithCString(0, C.CString("hive deck"), C.kCFStringEncodingUTF8)
		if st := C.MIDIClientCreate(name, nil, nil, &client); st != 0 {
			clientErr = fmt.Errorf("MIDIClientCreate: OSStatus %d", st)
		}
	})
	return client, clientErr
}

func objName(obj C.MIDIObjectRef) string {
	buf := (*C.char)(C.malloc(256))
	defer C.free(unsafe.Pointer(buf))
	if C.deckName(obj, buf, 256) == 0 {
		return ""
	}
	return C.GoString(buf)
}

func list() ([]string, error) {
	var out []string
	for i := C.ItemCount(0); i < C.MIDIGetNumberOfSources(); i++ {
		out = append(out, objName(C.MIDIObjectRef(C.MIDIGetSource(i)))+" (in)")
	}
	for i := C.ItemCount(0); i < C.MIDIGetNumberOfDestinations(); i++ {
		out = append(out, objName(C.MIDIObjectRef(C.MIDIGetDestination(i)))+" (out)")
	}
	return out, nil
}

type macPort struct {
	id     int
	name   string
	in     chan Msg
	parse  Parser
	inPort C.MIDIPortRef
	out    C.MIDIPortRef
	dst    C.MIDIEndpointRef
	closed bool
}

//export deckMidiRead
func deckMidiRead(id C.int, data *C.uchar, n C.int) {
	b := C.GoBytes(unsafe.Pointer(data), n)
	portsMu.Lock()
	defer portsMu.Unlock()
	p := ports[int(id)]
	if p == nil || p.closed {
		return
	}
	for _, m := range p.parse.Feed(b) {
		select {
		case p.in <- m:
		default:
		}
	}
}

func open(want string) (Port, error) {
	cl, err := midiClient()
	if err != nil {
		return nil, err
	}
	var src, dst C.MIDIEndpointRef
	var name string
	for i := C.ItemCount(0); i < C.MIDIGetNumberOfSources(); i++ {
		s := C.MIDIGetSource(i)
		if n := objName(C.MIDIObjectRef(s)); Match(n, want) {
			src, name = s, n
			break
		}
	}
	for i := C.ItemCount(0); i < C.MIDIGetNumberOfDestinations(); i++ {
		d := C.MIDIGetDestination(i)
		if Match(objName(C.MIDIObjectRef(d)), want) {
			dst = d
			break
		}
	}
	if src == 0 {
		return nil, fmt.Errorf("%w matching %q (is it plugged in? `deck list` shows what is there)", ErrNoDevice, want)
	}
	portsMu.Lock()
	p := &macPort{id: nextID, name: name, in: make(chan Msg, 256), dst: dst}
	nextID++
	ports[p.id] = p
	portsMu.Unlock()
	if st := C.deckOpenIn(cl, src, C.int(p.id), &p.inPort); st != 0 {
		return nil, fmt.Errorf("opening %s: OSStatus %d", name, st)
	}
	if dst != 0 {
		C.MIDIOutputPortCreate(cl, C.CFStringCreateWithCString(0, C.CString("deck out"), C.kCFStringEncodingUTF8), &p.out)
	}
	return p, nil
}

func (p *macPort) Name() string   { return p.name }
func (p *macPort) In() <-chan Msg { return p.in }

func (p *macPort) Send(m Msg) error {
	if p.dst == 0 || p.out == 0 {
		return nil
	}
	n := 1 + dataLen(m[0])
	if st := C.deckSend(p.out, p.dst, (*C.uchar)(unsafe.Pointer(&m[0])), C.int(n)); st != 0 {
		return fmt.Errorf("MIDISend: OSStatus %d", st)
	}
	return nil
}

func (p *macPort) Close() error {
	C.MIDIPortDispose(p.inPort)
	if p.out != 0 {
		C.MIDIPortDispose(p.out)
	}
	portsMu.Lock()
	defer portsMu.Unlock()
	if !p.closed {
		p.closed = true
		delete(ports, p.id)
		close(p.in)
	}
	return nil
}
