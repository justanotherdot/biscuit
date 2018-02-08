package main

import "fmt"
//import "math/rand"
import "runtime"
import "runtime/debug"
import "sync/atomic"
import "sync"
import "strings"
import "time"
import "unsafe"

import "common"
import "fs"

var	bsp_apic_id int

// these functions can only be used when interrupts are cleared
//go:nosplit
func lap_id() int {
	lapaddr := (*[1024]uint32)(unsafe.Pointer(uintptr(0xfee00000)))
	return int(lapaddr[0x20/4] >> 24)
}



var irqs int

// trapstub() cannot do anything that may have side-effects on the runtime
// (like allocate, fmt.Print, or use panic) since trapstub() runs in interrupt
// context and thus may run concurrently with code manipulating the same state.
// since trapstub() runs on the per-cpu interrupt stack, it must be nosplit.
//go:nosplit
func trapstub(tf *[common.TFSIZE]uintptr) {

	trapno := tf[TF_TRAP]

	// only IRQs come through here now
	if trapno <= TIMER || trapno > IRQ_LAST {
		runtime.Pnum(0x1001)
		for {}
	}

	irqs++
	switch trapno {
	case INT_KBD, INT_COM1:
		runtime.IRQwake(uint(trapno))
		// we need to mask the interrupt on the IOAPIC since my
		// hardware's LAPIC automatically send EOIs to IOAPICS when the
		// LAPIC receives an EOI and does not support disabling these
		// automatic EOI broadcasts (newer LAPICs do). its probably
		// better to disable automatic EOI broadcast and send EOIs to
		// the IOAPICs in the driver (as the code used to when using
		// 8259s).

		// masking the IRQ on the IO APIC must happen before writing
		// EOI to the LAPIC (otherwise the CPU will probably receive
		// another interrupt from the same IRQ). the LAPIC EOI happens
		// in the runtime...
		irqno := int(trapno - IRQ_BASE)
		apic.irq_mask(irqno)
	case uintptr(INT_DISK), INT_MSI0, INT_MSI1, INT_MSI2, INT_MSI3, INT_MSI4, INT_MSI5,
		INT_MSI6, INT_MSI7:
		// MSI dispatch doesn't use the IO APIC, thus no need for
		// irq_mask
		runtime.IRQwake(uint(trapno))
	default:
		// unexpected IRQ
		runtime.Pnum(int(trapno))
		runtime.Pnum(int(tf[TF_RIP]))
		runtime.Pnum(0xbadbabe)
		for {}
	}
}

var ide_int_done       = make(chan bool)

func trap_disk(intn uint) {
	for {
		runtime.IRQsched(intn)

		// is this a disk int?
		if !disk.intr() {
			fmt.Printf("spurious disk int\n")
			return
		}
		ide_int_done <- true
	}
}

func trap_cons(intn uint, ch chan bool) {
	for {
		runtime.IRQsched(intn)
		ch <- true
	}
}

func tfdump(tf *[common.TFSIZE]int) {
	fmt.Printf("RIP: %#x\n", tf[TF_RIP])
	fmt.Printf("RAX: %#x\n", tf[TF_RAX])
	fmt.Printf("RDI: %#x\n", tf[TF_RDI])
	fmt.Printf("RSI: %#x\n", tf[TF_RSI])
	fmt.Printf("RBX: %#x\n", tf[TF_RBX])
	fmt.Printf("RCX: %#x\n", tf[TF_RCX])
	fmt.Printf("RDX: %#x\n", tf[TF_RDX])
	fmt.Printf("RSP: %#x\n", tf[TF_RSP])
}

type dev_t struct {
	major	int
	minor	int
}

var dummyfops	= &devfops_t{maj: common.D_CONSOLE, min: 0}

// special fds
var fd_stdin 	= common.Fd_t{Fops: dummyfops, Perms: FD_READ}
var fd_stdout 	= common.Fd_t{Fops: dummyfops, Perms: FD_WRITE}
var fd_stderr 	= common.Fd_t{Fops: dummyfops, Perms: FD_WRITE}

var _deflimits = ulimit_t {
	// mem limit = 128 MB
	pages: (1 << 27) / (1 << 12),
	//nofile: 512,
	nofile: RLIM_INFINITY,
	novma: (1 << 8),
	noproc: (1 << 10),
}

// returns the new proc and success; can fail if the system-wide limit of
// procs/threads has been reached. the parent's fdtable must be locked.
func proc_new(name string, cwd *common.Fd_t, fds []*common.Fd_t) (*common.Proc_t, bool) {
	proclock.Lock()

	if nthreads >= int64(syslimit.sysprocs) {
		proclock.Unlock()
		return nil, false
	}

	nthreads++

	pid_cur++
	np := pid_cur
	pid_cur++
	tid0 := tid_t(pid_cur)
	if _, ok := allprocs[np]; ok {
		panic("pid exists")
	}
	ret := &common.Proc_t{}
	allprocs[np] = ret
	proclock.Unlock()

	ret.name = name
	ret.pid = np
	ret.fds = make([]*common.Fd_t, len(fds))
	ret.fdstart = 3
	for i := range fds {
		if fds[i] == nil {
			continue
		}
		tfd, err := copyfd(fds[i])
		// copying an fd may fail if another thread closes the fd out
		// from under us
		if err == 0 {
			ret.fds[i] = tfd
		}
		ret.nfds++
	}
	ret.cwd = cwd
	if ret.cwd.fops.reopen() != 0 {
		panic("must succeed")
	}
	ret.mmapi = USERMIN
	ret.ulim = _deflimits

	ret.threadi.init()
	ret.tid0 = tid0
	ret._thread_new(tid0)

	ret.mywait.wait_init(ret.pid)
	if !ret.start_thread(ret.tid0) {
		panic("silly noproc")
	}

	return ret, true
}



// interface for reading/writing from user space memory either via a pointer
// and length or an array of pointers and lengths (iovec)
type userio_i interface {
	// copy src to user memory
	uiowrite(src []uint8) (int, common.Err_t)
	// copy user memory to dst
	uioread(dst []uint8) (int, common.Err_t)
	// returns the number of unwritten/unread bytes remaining
	remain() int
	// the total buffer size
	totalsz() int
}

// a userio_i type that copies nothing. useful as an argument to {send,recv}msg
// when no from/to address or ancillary data is requested.
type _nilbuf_t struct {
}


func (nb *_nilbuf_t) uiowrite(src []uint8) (int, common.Err_t) {
	return 0, 0
}

func (nb *_nilbuf_t) uioread(dst []uint8) (int, common.Err_t) {
	return 0, 0
}

func (nb *_nilbuf_t) remain() int {
	return 0
}

func (nb *_nilbuf_t) totalsz() int {
	return 0
}

var zeroubuf = &_nilbuf_t{}

// helper type which kernel code can use as userio_i, but is actually a kernel
// buffer (i.e. reading an ELF header from the file system for exec(2)).
type fakeubuf_t struct {
	fbuf	[]uint8
	off	int
	len	int
}

func (fb *fakeubuf_t) fake_init(buf []uint8) {
	fb.fbuf = buf
	fb.len = len(fb.fbuf)
}

func (fb *fakeubuf_t) remain() int {
	return len(fb.fbuf)
}

func (fb *fakeubuf_t) totalsz() int {
	return fb.len
}

func (fb *fakeubuf_t) _tx(buf []uint8, tofbuf bool) (int, common.Err_t) {
	var c int
	if tofbuf {
		c = copy(fb.fbuf, buf)
	} else {
		c = copy(buf, fb.fbuf)
	}
	fb.fbuf = fb.fbuf[c:]
	return c, 0
}

func (fb *fakeubuf_t) uioread(dst []uint8) (int, common.Err_t) {
	return fb._tx(dst, false)
}

func (fb *fakeubuf_t) uiowrite(src []uint8) (int, common.Err_t) {
	return fb._tx(src, true)
}


type _iove_t struct {
	uva	uint
	sz	int
}

type useriovec_t struct {
	iovs	[]_iove_t
	tsz	int
	proc	*common.Proc_t
}

func (iov *useriovec_t) iov_init(proc *common.Proc_t, iovarn uint, niovs int) common.Err_t {
	if niovs > 10 {
		fmt.Printf("many iovecs\n")
		return -EINVAL
	}
	iov.tsz = 0
	iov.iovs = make([]_iove_t, niovs)
	iov.proc = proc

	proc.Lock_pmap()
	defer proc.Unlock_pmap()
	for i := range iov.iovs {
		elmsz := uint(16)
		va := iovarn + uint(i)*elmsz
		dstva, ok1 := proc.userreadn_inner(int(va), 8)
		sz, ok2    := proc.userreadn_inner(int(va) + 8, 8)
		if !ok1 || !ok2 {
			return -EFAULT
		}
		iov.iovs[i].uva = uint(dstva)
		iov.iovs[i].sz = sz
		iov.tsz += sz
	}
	return 0
}

func (iov *useriovec_t) remain() int {
	ret := 0
	for i := range iov.iovs {
		ret += iov.iovs[i].sz
	}
	return ret
}

func (iov *useriovec_t) totalsz() int {
	return iov.tsz
}

func (iov *useriovec_t) _tx(buf []uint8, touser bool) (int, common.Err_t) {
	ub := &userbuf_t{}
	did := 0
	for len(buf) > 0 && len(iov.iovs) > 0 {
		ciov := &iov.iovs[0]
		ub.ub_init(iov.proc, int(ciov.uva), ciov.sz)
		var c int
		var err common.Err_t
		if touser {
			c, err = ub._tx(buf, true)
		} else {
			c, err = ub._tx(buf, false)
		}
		ciov.uva += uint(c)
		ciov.sz -= c
		if ciov.sz == 0 {
			iov.iovs = iov.iovs[1:]
		}
		buf = buf[c:]
		did += c
		if err != 0 {
			return did, err
		}
	}
	return did, 0
}

func (iov *useriovec_t) uioread(dst []uint8) (int, common.Err_t) {
	iov.proc.Lock_pmap()
	a, b := iov._tx(dst, false)
	iov.proc.Unlock_pmap()
	return a, b
}

func (iov *useriovec_t) uiowrite(src []uint8) (int, common.Err_t) {
	iov.proc.Lock_pmap()
	a, b := iov._tx(src, true)
	iov.proc.Unlock_pmap()
	return a, b
}

// a circular buffer that is read/written by userspace. not thread-safe -- it
// is intended to be used by one daemon.
type circbuf_t struct {
	buf	[]uint8
	bufsz	int
	// XXX uint
	head	int
	tail	int
	p_pg	common.Pa_t
}

// may fail to allocate a page for the buffer. when cb's life is over, someone
// must free the buffer page by calling cb_release().
func (cb *circbuf_t) cb_init(sz int) common.Err_t {
	bufmax := int(PGSIZE)
	if sz <= 0 || sz > bufmax {
		panic("bad circbuf size")
	}
	cb.bufsz = sz
	cb.head, cb.tail = 0, 0
	// lazily allocated the buffers. it is easier to handle an error at the
	// time of read or write instead of during the initialization of the
	// object using a circbuf.
	return 0
}

// provide the page for the buffer explicitly; useful for guaranteeing that
// read/writes won't fail to allocate memory.
func (cb *circbuf_t) cb_init_phys(v []uint8, p_pg common.Pa_t) {
	refup(p_pg)
	cb.p_pg = p_pg
	cb.buf = v
	cb.bufsz = len(cb.buf)
	cb.head, cb.tail = 0, 0
}

func (cb *circbuf_t) cb_release() {
	if cb.buf == nil {
		return
	}
	refdown(cb.p_pg)
	cb.p_pg = 0
	cb.buf = nil
	cb.head, cb.tail = 0, 0
}

func (cb *circbuf_t) cb_ensure() common.Err_t {
	if cb.buf != nil {
		return 0
	}
	if cb.bufsz == 0 {
		panic("not initted")
	}
	pg, p_pg, ok := refpg_new_nozero()
	if !ok {
		return -ENOMEM
	}
	bpg := pg2bytes(pg)[:]
	bpg = bpg[:cb.bufsz]
	cb.cb_init_phys(bpg, p_pg)
	return 0
}

func (cb *circbuf_t) full() bool {
	return cb.head - cb.tail == cb.bufsz
}

func (cb *circbuf_t) empty() bool {
	return cb.head == cb.tail
}

func (cb *circbuf_t) left() int {
	used := cb.head - cb.tail
	rem := cb.bufsz - used
	return rem
}

func (cb *circbuf_t) used() int {
	used := cb.head - cb.tail
	return used
}

func (cb *circbuf_t) copyin(src userio_i) (int, common.Err_t) {
	if err := cb.cb_ensure(); err != 0 {
		return 0, err
	}
	if cb.full() {
		return 0, 0
	}
	hi := cb.head % cb.bufsz
	ti := cb.tail % cb.bufsz
	c := 0
	// wraparound?
	if ti <= hi {
		dst := cb.buf[hi:]
		wrote, err := src.uioread(dst)
		if err != 0 {
			return 0, err
		}
		if wrote != len(dst) {
			cb.head += wrote
			return wrote, 0
		}
		c += wrote
		hi = (cb.head + wrote) % cb.bufsz
	}
	// XXXPANIC
	if hi > ti {
		panic("wut?")
	}
	dst := cb.buf[hi:ti]
	wrote, err := src.uioread(dst)
	c += wrote
	if err != 0 {
		return c, err
	}
	cb.head += c
	return c, 0
}

func (cb *circbuf_t) copyout(dst userio_i) (int, common.Err_t) {
	return cb.copyout_n(dst, 0)
}

func (cb *circbuf_t) copyout_n(dst userio_i, max int) (int, common.Err_t) {
	if err := cb.cb_ensure(); err != 0 {
		return 0, err
	}
	if cb.empty() {
		return 0, 0
	}
	hi := cb.head % cb.bufsz
	ti := cb.tail % cb.bufsz
	c := 0
	// wraparound?
	if hi <= ti {
		src := cb.buf[ti:]
		if max != 0 && max < len(src) {
			src = src[:max]
		}
		wrote, err := dst.uiowrite(src)
		if err != 0 {
			return 0, err
		}
		if wrote != len(src) || wrote == max {
			cb.tail += wrote
			return wrote, 0
		}
		c += wrote
		if max != 0 {
			max -= c
		}
		ti = (cb.tail + wrote) % cb.bufsz
	}
	// XXXPANIC
	if ti > hi {
		panic("wut?")
	}
	src := cb.buf[ti:hi]
	if max != 0 && max < len(src) {
		src = src[:max]
	}
	wrote, err := dst.uiowrite(src)
	if err != 0 {
		return 0, err
	}
	c += wrote
	cb.tail += c
	return c, 0
}

// returns slices referencing the internal circular buffer [head+offset,
// head+offset+sz) which must be outside [tail, head). returns two slices when
// the returned buffer wraps.
// XXX XXX XXX XXX XXX remove arg
func (cb *circbuf_t) _rawwrite(offset, sz int) ([]uint8, []uint8) {
	if cb.buf == nil {
		panic("no lazy allocation for tcp")
	}
	if cb.left() < sz {
		panic("bad size")
	}
	if sz == 0 {
		return nil, nil
	}
	oi := (cb.head + offset) % cb.bufsz
	oe := (cb.head + offset + sz) % cb.bufsz
	hi := cb.head % cb.bufsz
	ti := cb.tail % cb.bufsz
	var r1 []uint8
	var r2 []uint8
	if ti <= hi {
		if (oi >= ti && oi < hi) || (oe > ti && oe <= hi) {
			panic("intersects with user data")
		}
		r1 = cb.buf[oi:]
		if len(r1) > sz {
			r1 = r1[:sz]
		} else {
			r2 = cb.buf[:oe]
		}
	} else {
		// user data wraps
		if !(oi >= hi && oi < ti && oe > hi && oe <= ti) {
			panic("intersects with user data")
		}
		r1 = cb.buf[oi:oe]
	}
	return r1, r2
}

// advances head index sz bytes (allowing the bytes to be copied out)
func (cb *circbuf_t) _advhead(sz int) {
	if cb.full() || cb.left() < sz {
		panic("advancing full cb")
	}
	cb.head += sz
}

// returns slices referencing the circular buffer [tail+offset, tail+offset+sz)
// which must be inside [tail, head). returns two slices when the returned
// buffer wraps.
func (cb *circbuf_t) _rawread(offset int) ([]uint8, []uint8) {
	if cb.buf == nil {
		panic("no lazy allocation for tcp")
	}
	oi := (cb.tail + offset) % cb.bufsz
	hi := cb.head % cb.bufsz
	ti := cb.tail % cb.bufsz
	var r1 []uint8
	var r2 []uint8
	if ti < hi {
		if oi >= hi || oi < ti {
			panic("outside user data")
		}
		r1 = cb.buf[oi:hi]
	} else {
		if oi >= hi && oi < ti {
			panic("outside user data")
		}
		tlen := len(cb.buf[ti:])
		if tlen > offset {
			r1 = cb.buf[oi:]
			r2 = cb.buf[:hi]
		} else {
			roff := offset - tlen
			r1 = cb.buf[roff:hi]
		}
	}
	return r1, r2
}

// advances head index sz bytes (allowing the bytes to be copied out)
func (cb *circbuf_t) _advtail(sz int) {
	if sz != 0 && (cb.empty() || cb.used() < sz) {
		panic("advancing empty cb")
	}
	cb.tail += sz
}

type passfd_t struct {
	cb	[]*common.Fd_t
	inum	uint
	cnum	uint
}

func (pf *passfd_t) add(nfd *common.Fd_t) bool {
	if pf.cb == nil {
		pf.cb = make([]*common.Fd_t, 10)
	}
	l := uint(len(pf.cb))
	if pf.inum - pf.cnum == l {
		return false
	}
	pf.cb[pf.inum % l] = nfd
	pf.inum++
	return true
}

func (pf *passfd_t) take() (*common.Fd_t, bool) {
	l := uint(len(pf.cb))
	if pf.inum == pf.cnum {
		return nil, false
	}
	ret := pf.cb[pf.cnum % l]
	pf.cnum++
	return ret, true
}

func (pf *passfd_t) closeall() {
	for {
		fd, ok := pf.take()
		if !ok {
			break
		}
		fd.fops.close()
	}
}

func cpus_stack_init(apcnt int, stackstart uintptr) {
	for i := 0; i < apcnt; i++ {
		// allocate/map interrupt stack
		kmalloc(stackstart, PTE_W)
		stackstart += PGSIZEW
		assert_no_va_map(kpmap(), stackstart)
		stackstart += PGSIZEW
		// allocate/map NMI stack
		kmalloc(stackstart, PTE_W)
		stackstart += PGSIZEW
		assert_no_va_map(kpmap(), stackstart)
		stackstart += PGSIZEW
	}
}

func cpus_start(ncpu, aplim int) {
	runtime.GOMAXPROCS(1 + aplim)
	apcnt := ncpu - 1

	fmt.Printf("found %v CPUs\n", ncpu)

	if apcnt == 0 {
		fmt.Printf("uniprocessor\n")
		return
	}

	// AP code must be between 0-1MB because the APs are in real mode. load
	// code to 0x8000 (overwriting bootloader)
	mpaddr := common.Pa_t(0x8000)
	mpcode := allbins["mpentry.bin"].data
	c := common.Pa_t(0)
	mpl := common.Pa_t(len(mpcode))
	for c < mpl {
		mppg := dmap8(mpaddr + c)
		did := copy(mppg, mpcode)
		mpcode = mpcode[did:]
		c += common.Pa_t(did)
	}

	// skip mucking with CMOS reset code/warm reset vector (as per the the
	// "universal startup algoirthm") and instead use the STARTUP IPI which
	// is supported by lapics of version >= 1.x. (the runtime panicks if a
	// lapic whose version is < 1.x is found, thus assume their absence).
	// however, only one STARTUP IPI is accepted after a CPUs RESET or INIT
	// pin is asserted, thus we need to send an INIT IPI assert first (it
	// appears someone already used a STARTUP IPI; probably the BIOS).

	lapaddr := 0xfee00000
	pte := pmap_lookup(kpmap(), lapaddr)
	if pte == nil || *pte & PTE_P == 0 || *pte & PTE_PCD == 0 {
		panic("lapaddr unmapped")
	}
	lap := (*[PGSIZE/4]uint32)(unsafe.Pointer(uintptr(lapaddr)))
	icrh := 0x310/4
	icrl := 0x300/4

	ipilow := func(ds int, t int, l int, deliv int, vec int) uint32 {
		return uint32(ds << 18 | t << 15 | l << 14 |
		    deliv << 8 | vec)
	}

	icrw := func(hi uint32, low uint32) {
		// use sync to guarantee order
		atomic.StoreUint32(&lap[icrh], hi)
		atomic.StoreUint32(&lap[icrl], low)
		ipisent := uint32(1 << 12)
		for atomic.LoadUint32(&lap[icrl]) & ipisent != 0 {
		}
	}

	// destination shorthands:
	// 1: self
	// 2: all
	// 3: all but me

	initipi := func(assert bool) {
		vec := 0
		delivmode := 0x5
		level := 1
		trig  := 0
		dshort:= 3
		if !assert {
			trig = 1
			level = 0
			dshort = 2
		}
		hi  := uint32(0)
		low := ipilow(dshort, trig, level, delivmode, vec)
		icrw(hi, low)
	}

	startupipi := func() {
		vec       := int(mpaddr >> 12)
		delivmode := 0x6
		level     := 0x1
		trig      := 0x0
		dshort    := 0x3

		hi := uint32(0)
		low := ipilow(dshort, trig, level, delivmode, vec)
		icrw(hi, low)
	}

	// pass arguments to the ap startup code via secret storage (the old
	// boot loader page at 0x7c00)

	// secret storage layout
	// 0 - e820map
	// 1 - pmap
	// 2 - firstfree
	// 3 - ap entry
	// 4 - gdt
	// 5 - gdt
	// 6 - idt
	// 7 - idt
	// 8 - ap count
	// 9 - stack start
	// 10- proceed

	ss := (*[11]uintptr)(unsafe.Pointer(uintptr(0x7c00)))
	sap_entry := 3
	sgdt      := 4
	sidt      := 6
	sapcnt    := 8
	sstacks   := 9
	sproceed  := 10
	var _dur func(uint)
	_dur = ap_entry
	ss[sap_entry] = **(**uintptr)(unsafe.Pointer(&_dur))
	// sgdt and sidt save 10 bytes
	runtime.Sgdt(&ss[sgdt])
	runtime.Sidt(&ss[sidt])
	atomic.StoreUintptr(&ss[sapcnt], 0)
	// for BSP:
	// 	int stack	[0xa100000000, 0xa100001000)
	// 	guard page	[0xa100001000, 0xa100002000)
	// 	NMI stack	[0xa100002000, 0xa100003000)
	// 	guard page	[0xa100003000, 0xa100004000)
	// for each AP:
	// 	int stack	BSP's + apnum*4*PGSIZE + 0*PGSIZE
	// 	NMI stack	BSP's + apnum*4*PGSIZE + 2*PGSIZE
	stackstart := uintptr(0xa100004000)
	// each ap grabs a unique stack
	atomic.StoreUintptr(&ss[sstacks], stackstart)
	atomic.StoreUintptr(&ss[sproceed], 0)

	dummy := int64(0)
	atomic.CompareAndSwapInt64(&dummy, 0, 10)

	initipi(true)
	// not necessary since we assume lapic version >= 1.x (ie not 82489DX)
	//initipi(false)
	time.Sleep(10*time.Millisecond)

	startupipi()
	time.Sleep(10*time.Millisecond)
	startupipi()

	// wait a while for hopefully all APs to join.
	time.Sleep(500*time.Millisecond)
	apcnt = int(atomic.LoadUintptr(&ss[sapcnt]))
	if apcnt > aplim {
		apcnt = aplim
	}
	set_cpucount(apcnt + 1)

	// actually map the stacks for the CPUs that joined
	cpus_stack_init(apcnt, stackstart)

	// tell the cpus to carry on
	atomic.StoreUintptr(&ss[sproceed], uintptr(apcnt))

	fmt.Printf("done! %v APs found (%v joined)\n", ss[sapcnt], apcnt)
}

// myid is a logical id, not lapic id
//go:nosplit
func ap_entry(myid uint) {

	// myid starts from 1
	runtime.Ap_setup(myid)

	// ints are still cleared. wait for timer int to enter scheduler
	fl := runtime.Pushcli()
	fl |= TF_FL_IF
	runtime.Popcli(fl)
	for {}
}

func set_cpucount(n int) {
	numcpus = n
	runtime.Setncpu(int32(n))
}

func irq_unmask(irq int) {
	apic.irq_unmask(irq)
}

func irq_eoi(irq int) {
	//apic.eoi(irq)
	apic.irq_unmask(irq)
}

func kbd_init() {
	km := make(map[int]byte)
	NO := byte(0)
	tm := []byte{
	    // ty xv6
	    NO,   0x1B, '1',  '2',  '3',  '4',  '5',  '6',  // 0x00
	    '7',  '8',  '9',  '0',  '-',  '=',  '\b', '\t',
	    'q',  'w',  'e',  'r',  't',  'y',  'u',  'i',  // 0x10
	    'o',  'p',  '[',  ']',  '\n', NO,   'a',  's',
	    'd',  'f',  'g',  'h',  'j',  'k',  'l',  ';',  // 0x20
	    '\'', '`',  NO,   '\\', 'z',  'x',  'c',  'v',
	    'b',  'n',  'm',  ',',  '.',  '/',  NO,   '*',  // 0x30
	    NO,   ' ',  NO,   NO,   NO,   NO,   NO,   NO,
	    NO,   NO,   NO,   NO,   NO,   NO,   NO,   '7',  // 0x40
	    '8',  '9',  '-',  '4',  '5',  '6',  '+',  '1',
	    '2',  '3',  '0',  '.',  NO,   NO,   NO,   NO,   // 0x50
	    }

	for i, c := range tm {
		if c != NO {
			km[i] = c
		}
	}
	cons.kbd_int = make(chan bool)
	cons.com_int = make(chan bool)
	cons.reader = make(chan []byte)
	cons.reqc = make(chan int)
	cons.pollc = make(chan pollmsg_t)
	cons.pollret = make(chan ready_t)
	go kbd_daemon(&cons, km)
	irq_unmask(IRQ_KBD)
	irq_unmask(IRQ_COM1)

	// make sure kbd int and com1 int are clear
	for _kready() {
		runtime.Inb(0x60)
	}
	for _comready() {
		runtime.Inb(0x3f8)
	}

	go trap_cons(INT_KBD, cons.kbd_int)
	go trap_cons(INT_COM1, cons.com_int)
}

type cons_t struct {
	kbd_int		chan bool
	com_int		chan bool
	reader		chan []byte
	reqc		chan int
	pollc		chan common.Pollmsg_t
	pollret		chan common.Ready_t
}

var cons	= cons_t{}

func _comready() bool {
	com1ctl := uint16(0x3f8 + 5)
	b := runtime.Inb(com1ctl)
	if b & 0x01 == 0 {
		return false
	}
	return true
}

func _kready() bool {
	ibf := uint(1 << 0)
	st := runtime.Inb(0x64)
	if st & ibf == 0 {
		//panic("no kbd data?")
		return false
	}
	return true
}

func netdump() {
	fmt.Printf("net dump\n")
	tcpcons.l.Lock()
	fmt.Printf("tcp table len: %v\n", len(tcpcons.econns))
	//for _, tcb := range tcpcons.econns {
	//	tcb.l.Lock()
	//	//if tcb.state == TIMEWAIT {
	//	//	tcb.l.Unlock()
	//	//	continue
	//	//}
	//	fmt.Printf("%v:%v -> %v:%v: %s\n",
	//	    ip2str(tcb.lip), tcb.lport,
	//	    ip2str(tcb.rip), tcb.rport,
	//	    statestr[tcb.state])
	//	tcb.l.Unlock()
	//}
	tcpcons.l.Unlock()
}

func loping() {
	fmt.Printf("POING\n")
	sip, dip, err := routetbl.lookup(ip4_t(0x7f000001))
	if err != 0 {
		panic("error")
	}
	dmac, err := arp_resolve(sip, dip)
	if err != 0 {
		panic("error")
	}
	nic, ok := nic_lookup(sip)
	if !ok {
		panic("not ok")
	}
	pkt := &icmppkt_t{}
	data := make([]uint8, 8)
	writen(data, 8, 0, int(time.Now().UnixNano()))
	pkt.init(nic.lmac(), dmac, sip, dip, 8, data)
	pkt.ident = 0
	pkt.seq = 0
	pkt.crc()
	sgbuf := [][]uint8{pkt.hdrbytes(), data}
	nic.tx_ipv4(sgbuf)
}

func sizedump() {
	is := unsafe.Sizeof(int(0))
	condsz := unsafe.Sizeof(sync.Cond{})

	//pollersz := unsafe.Sizeof(pollers_t{}) + 10*unsafe.Sizeof(pollmsg_t{})
	var tf [common.TFSIZE]uintptr
	var fx [64]uintptr
	tfsz := unsafe.Sizeof(tf)
	fxsz := unsafe.Sizeof(fx)
	waitsz := uintptr(1e9)
	tnotesz := is
	timer := uintptr(2*8 + 8*8)
	polls := unsafe.Sizeof(pollers_t{}) + 10*(unsafe.Sizeof(pollmsg_t{}) + timer)
	fdsz := unsafe.Sizeof(common.Fd_t{})
	mfile := unsafe.Sizeof(mfile_t{})
	// add fops, pollers_t, Conds

	fmt.Printf("in bytes\n")
	fmt.Printf("ARP rec: %v + 1map\n", unsafe.Sizeof(arprec_t{}))
	fmt.Printf("dentry : %v\n", unsafe.Sizeof(dc_rbn_t{}))
	fmt.Printf("futex  : %v + stack\n", unsafe.Sizeof(futex_t{}))
	fmt.Printf("route  : %v + 1map\n", unsafe.Sizeof(rtentry_t{}) + is)

	// XXX account for block and inode cache

	// dirtyarray := uintptr(8)
	//fmt.Printf("mfs    : %v\n", uintptr(2*8 + dirtyarray) +
	//	unsafe.Sizeof(frbn_t{}) + unsafe.Sizeof(pginfo_t{}))

	fmt.Printf("vnode  : %v + 1map\n", unsafe.Sizeof(imemnode_t{}) +
		unsafe.Sizeof(bdev_block_t{}) + 512 + condsz +
		unsafe.Sizeof(fsfops_t{}))
	fmt.Printf("pipe   : %v\n", unsafe.Sizeof(pipe_t{}) +
		unsafe.Sizeof(pipefops_t{}) + 2*condsz)
	fmt.Printf("process: %v + stack + wait\n", unsafe.Sizeof(common.Proc_t{}) +
		tfsz + fxsz + waitsz + tnotesz + timer)
	fmt.Printf("\tvma    : %v\n", unsafe.Sizeof(rbn_t{}) + mfile)
	fmt.Printf("\t1 RBfd : %v\n", unsafe.Sizeof(frbn_t{}))
	fmt.Printf("\t1 fd   : %v\n", fdsz)
	fmt.Printf("\tper-dev poll md: %v\n", polls)
	fmt.Printf("TCB    : %v + 1map\n", unsafe.Sizeof(tcptcb_t{}) +
		unsafe.Sizeof(tcpkey_t{}) + unsafe.Sizeof(tcpfops_t{}) +
		2*condsz + timer)
	fmt.Printf("LTCB   : %v + 1map\n", unsafe.Sizeof(tcplisten_t{}) +
		unsafe.Sizeof(tcplkey_t{}) + is + unsafe.Sizeof(tcplfops_t{}) +
		timer)
	fmt.Printf("US sock: %v + 1map\n", unsafe.Sizeof(susfops_t{}) +
		2*unsafe.Sizeof(pipefops_t{}) + unsafe.Sizeof(pipe_t{}) + 2*condsz)
	fmt.Printf("UD sock: %v + 1map\n", unsafe.Sizeof(sudfops_t{}) +
		unsafe.Sizeof(bud_t{}) + condsz + uintptr(PGSIZE)/10)
}

var _nflip int

func kbd_daemon(cons *cons_t, km map[int]byte) {
	inb := runtime.Inb
	start := make([]byte, 0, 10)
	data := start
	addprint := func(c byte) {
		fmt.Printf("%c", c)
		if len(data) > 1024 {
			fmt.Printf("key dropped!\n")
			return
		}
		data = append(data, c)
		if c == '\\' {
			debug.SetTraceback("all")
			panic("yahoo")
		} else if c == '@' {

		} else if c == '%' {
			//loping()
			//netdump()

			//bp := &bprof_t{}
			//err := pprof.WriteHeapProfile(bp)
			//if err != nil {
			//	fmt.Printf("shat on: %v\n", err)
			//} else {
			//	bp.dump()
			//	fmt.Printf("success?\n")
			//}

		}
	}
	var reqc chan int
	pollers := &pollers_t{}
	for {
		select {
		case <- cons.kbd_int:
			for _kready() {
				sc := int(inb(0x60))
				c, ok := km[sc]
				if ok {
					addprint(c)
				}
			}
			irq_eoi(IRQ_KBD)
		case <- cons.com_int:
			for _comready() {
				com1data := uint16(0x3f8 + 0)
				sc := inb(com1data)
				c := byte(sc)
				if c == '\r' {
					c = '\n'
				} else if c == 127 {
					// delete -> backspace
					c = '\b'
				}
				addprint(c)
			}
			irq_eoi(IRQ_COM1)
		case l := <- reqc:
			if l > len(data) {
				l = len(data)
			}
			s := data[0:l]
			cons.reader <- s
			data = data[l:]
		case pm := <- cons.pollc:
			if pm.events & R_READ == 0 {
				cons.pollret <- 0
				continue
			}
			var ret ready_t
			if len(data) > 0 {
				ret |= R_READ
			} else if pm.dowait {
				pollers.addpoller(&pm)
			}
			cons.pollret <- ret
		}
		if len(data) == 0 {
			reqc = nil
			data = start
		} else {
			reqc = cons.reqc
			pollers.wakeready(R_READ)
		}
	}
}

// reads keyboard data, blocking for at least 1 byte. returns at most cnt
// bytes.
func kbd_get(cnt int) []byte {
	if cnt < 0 {
		panic("negative cnt")
	}
	cons.reqc <- cnt
	return <- cons.reader
}

func attach_devs() int {
	ncpu := acpi_attach()
	pcibus_attach()
	return ncpu
}


type bprof_t struct {
	data	[]byte
}

func (b *bprof_t) init() {
	b.data = make([]byte, 0, 4096)
}

func (b *bprof_t) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *bprof_t) len() int {
	return len(b.data)
}

// dumps profile to serial console/vga for xxd -r
func (b *bprof_t) dump() {
	hexdump(b.data)
}

func hexdump(buf []uint8) {
	l := len(buf)
	for i := 0; i < l; i += 16 {
		cur := buf[i:]
		if len(cur) > 16 {
			cur = cur[:16]
		}
		fmt.Printf("%07x: ", i)
		prc := 0
		for _, b := range cur {
			fmt.Printf("%02x", b)
			prc++
			if prc % 2 == 0 {
				fmt.Printf(" ")
			}
		}
		fmt.Printf("\n")
	}
}

var prof = bprof_t{}

func cpuidfamily() (uint, uint) {
	ax, _, _, _ := runtime.Cpuid(1, 0)
	model :=  (ax >> 4) & 0xf
	family := (ax >> 8) & 0xf
	emodel := (ax >> 16) & 0xf
	efamily := (ax >> 20) & 0xff

	dispmodel := emodel << 4 + model
	dispfamily := efamily + family
	return uint(dispmodel), uint(dispfamily)
}

func cpuchk() {
	_, _, _, dx := runtime.Cpuid(0x80000001, 0)
	arch64 := uint32(1 << 29)
	if dx & arch64 == 0 {
		panic("not intel 64 arch?")
	}

	rmodel, rfamily := cpuidfamily()
	fmt.Printf("CPUID: family: %x, model: %x\n", rfamily, rmodel)

	ax, _, _, dx := runtime.Cpuid(1, 0)
	stepping := ax & 0xf
	oldp := rfamily == 6 && rmodel < 3 && stepping < 3
	sep := uint32(1 << 11)
	if dx & sep == 0 || oldp {
		panic("sysenter not supported")
	}

	_, _, _, dx = runtime.Cpuid(0x80000007, 0)
	invartsc := uint32(1 << 8)
	if dx & invartsc == 0 {
		// no qemu CPUs support invariant tsc, but my hardware does...
		//panic("invariant tsc not supported")
		fmt.Printf("invariant TSC not supported\n")
	}
}

func perfsetup() {
	ax, bx, _, _ := runtime.Cpuid(0xa, 0)
	perfv := ax & 0xff
	npmc := (ax >> 8) & 0xff
	pmcbits := (ax >> 16) & 0xff
	pmebits := (ax >> 24) & 0xff
	cyccnt := bx & 1 == 0
	_, _, cx, _ := runtime.Cpuid(0x1, 0)
	pdc := cx & (1 << 15) != 0
	if pdc && perfv >= 2 && perfv <= 3 && npmc >= 1 && pmebits >= 1 &&
	    cyccnt && pmcbits >= 32 {
		fmt.Printf("Hardware Performance monitoring enabled: " +
		    "%v counters\n", npmc)
		profhw = &intelprof_t{}
		profhw.prof_init(uint(npmc))
	} else {
		fmt.Printf("No hardware performance monitoring\n")
		profhw = &nilprof_t{}
	}
}

// performance monitoring event id
type pmevid_t uint

const(
	// if you modify the order of these flags, you must update them in libc
	// too.
	// architectural
	EV_UNHALTED_CORE_CYCLES		pmevid_t = 1 << iota
	EV_LLC_MISSES			pmevid_t = 1 << iota
	EV_LLC_REFS			pmevid_t = 1 << iota
	EV_BRANCH_INSTR_RETIRED		pmevid_t = 1 << iota
	EV_BRANCH_MISS_RETIRED		pmevid_t = 1 << iota
	EV_INSTR_RETIRED		pmevid_t = 1 << iota
	// non-architectural
	// "all TLB misses that cause a page walk"
	EV_DTLB_LOAD_MISS_ANY		pmevid_t = 1 << iota
	// "number of completed walks due to miss in sTLB"
	EV_DTLB_LOAD_MISS_STLB		pmevid_t = 1 << iota
	// "retired stores that missed in the dTLB"
	EV_STORE_DTLB_MISS		pmevid_t = 1 << iota
	EV_L2_LD_HITS			pmevid_t = 1 << iota
	// "Counts the number of misses in all levels of the ITLB which causes
	// a page walk."
	EV_ITLB_LOAD_MISS_ANY		pmevid_t = 1 << iota
)

type pmflag_t uint

const(
	EVF_OS				pmflag_t = 1 << iota
	EVF_USR				pmflag_t = 1 << iota
)

type pmev_t struct {
	evid	pmevid_t
	pflags	pmflag_t
}

var pmevid_names = map[pmevid_t]string{
	EV_UNHALTED_CORE_CYCLES: "Unhalted core cycles",
	EV_LLC_MISSES: "LLC misses",
	EV_LLC_REFS: "LLC references",
	EV_BRANCH_INSTR_RETIRED: "Branch instructions retired",
	EV_BRANCH_MISS_RETIRED: "Branch misses retired",
	EV_INSTR_RETIRED: "Instructions retired",
	EV_DTLB_LOAD_MISS_ANY: "dTLB load misses",
	EV_ITLB_LOAD_MISS_ANY: "iTLB load misses",
	EV_DTLB_LOAD_MISS_STLB: "sTLB misses",
	EV_STORE_DTLB_MISS: "Store dTLB misses",
	//EV_WTF1: "dummy 1",
	//EV_WTF2: "dummy 2",
	EV_L2_LD_HITS: "L2 load hits",
}

// a device driver for hardware profiling
type profhw_i interface {
	prof_init(uint)
	startpmc([]pmev_t) ([]int, bool)
	stoppmc([]int) []uint
	startnmi(pmevid_t, pmflag_t, uint, uint) bool
	stopnmi() []uintptr
}

var profhw profhw_i

type nilprof_t struct {
}

func (n *nilprof_t) prof_init(uint) {
}

func (n *nilprof_t) startpmc([]pmev_t) ([]int, bool) {
	return nil, false
}

func (n *nilprof_t) stoppmc([]int) []uint {
	return nil
}

func (n *nilprof_t) startnmi(pmevid_t, pmflag_t, uint, uint) bool {
	return false
}

func (n *nilprof_t) stopnmi() []uintptr {
	return nil
}

type intelprof_t struct {
	l		sync.Mutex
	pmcs		[]intelpmc_t
	events		map[pmevid_t]pmevent_t
}

type intelpmc_t struct {
	alloced		bool
	eventid		pmevid_t
}

type pmevent_t struct {
	event	int
	umask	int
}

func (ip *intelprof_t) _disableall() {
	ip._perfmaskipi()
}

func (ip *intelprof_t) _enableall() {
	ip._perfmaskipi()
}

func (ip *intelprof_t) _perfmaskipi() {
	lapaddr := 0xfee00000
	lap := (*[PGSIZE/4]uint32)(unsafe.Pointer(uintptr(lapaddr)))

	allandself := 2
	trap_perfmask := 72
	level := 1 << 14
	low := uint32(allandself << 18 | level | trap_perfmask)
	icrl := 0x300/4
	atomic.StoreUint32(&lap[icrl], low)
	ipisent := uint32(1 << 12)
	for atomic.LoadUint32(&lap[icrl]) & ipisent != 0 {
	}
}

func (ip *intelprof_t) _ev2msr(eid pmevid_t, pf pmflag_t) int {
	ev, ok := ip.events[eid]
	if !ok {
		panic("no such event")
	}
	usr := 1 << 16
	os  := 1 << 17
	en  := 1 << 22
	event := ev.event
	umask := ev.umask << 8
	v := umask | event | en
	if pf & EVF_OS != 0 {
		v |= os
	}
	if pf & EVF_USR != 0 {
		v |= usr
	}
	if pf == 0 {
		v |= os | usr
	}
	return v
}

// XXX counting PMCs only works with one CPU; move counter start/stop to perf
// IPI.
func (ip *intelprof_t) _pmc_start(cid int, eid pmevid_t, pf pmflag_t) {
	if cid < 0 || cid >= len(ip.pmcs) {
		panic("wtf")
	}
	wrmsr := func(a, b int) {
		runtime.Wrmsr(a, b)
	}
	ia32_pmc0 := 0xc1
	ia32_perfevtsel0 := 0x186
	pmc := ia32_pmc0 + cid
	evtsel := ia32_perfevtsel0 + cid
	// disable perf counter before clearing
	wrmsr(evtsel, 0)
	wrmsr(pmc, 0)

	v := ip._ev2msr(eid, pf)
	wrmsr(evtsel, v)
}

func (ip *intelprof_t) _pmc_stop(cid int) uint {
	if cid < 0 || cid >= len(ip.pmcs) {
		panic("wtf")
	}
	ia32_pmc0 := 0xc1
	ia32_perfevtsel0 := 0x186
	pmc := ia32_pmc0 + cid
	evtsel := ia32_perfevtsel0 + cid
	ret := runtime.Rdmsr(pmc)
	runtime.Wrmsr(evtsel, 0)
	return uint(ret)
}

func (ip *intelprof_t) prof_init(npmc uint) {
	ip.pmcs = make([]intelpmc_t, npmc)
	// architectural events
	ip.events = map[pmevid_t]pmevent_t{
	    EV_UNHALTED_CORE_CYCLES:
		{0x3c, 0},
	    EV_LLC_MISSES:
		{0x2e, 0x41},
	    EV_LLC_REFS:
		{0x2e, 0x4f},
	    EV_BRANCH_INSTR_RETIRED:
		{0xc4, 0x0},
	    EV_BRANCH_MISS_RETIRED:
		{0xc5, 0x0},
	    EV_INSTR_RETIRED:
		{0xc0, 0x0},
	}

	_xeon5000 := map[pmevid_t]pmevent_t{
	    EV_DTLB_LOAD_MISS_ANY:
		{0x08, 0x1},
	    EV_DTLB_LOAD_MISS_STLB:
		{0x08, 0x2},
	    EV_STORE_DTLB_MISS:
		{0x0c, 0x1},
	    EV_ITLB_LOAD_MISS_ANY:
		{0x85, 0x1},
	    //EV_WTF1:
	    //    {0x49, 0x1},
	    //EV_WTF2:
	    //    {0x14, 0x2},
	    EV_L2_LD_HITS:
		{0x24, 0x1},
	}

	dispmodel, dispfamily := cpuidfamily()

	if dispfamily == 0x6 && dispmodel == 0x1e {
		for k, v := range _xeon5000 {
			ip.events[k] = v
		}
	}
}

// starts a performance counter for each event in evs. if all the counters
// cannot be allocated, no performance counter is started.
func (ip *intelprof_t) startpmc(evs []pmev_t) ([]int, bool) {
	ip.l.Lock()
	defer ip.l.Unlock()

	// are the event ids supported?
	for _, ev := range evs {
		if _, ok := ip.events[ev.evid]; !ok {
			return nil, false
		}
	}
	// make sure we have enough counters
	cnt := 0
	for i := range ip.pmcs {
		if !ip.pmcs[i].alloced {
			cnt++
		}
	}
	if cnt < len(evs) {
		return nil, false
	}

	ret := make([]int, len(evs))
	ri := 0
	// find available counter
	outer:
	for _, ev := range evs {
		eid := ev.evid
		for i := range ip.pmcs {
			if !ip.pmcs[i].alloced {
				ip.pmcs[i].alloced = true
				ip.pmcs[i].eventid = eid
				ip._pmc_start(i, eid, ev.pflags)
				ret[ri] = i
				ri++
				continue outer
			}
		}
	}
	return ret, true
}

func (ip *intelprof_t) stoppmc(idxs []int) []uint {
	ip.l.Lock()
	defer ip.l.Unlock()

	ret := make([]uint, len(idxs))
	ri := 0
	for _, idx := range idxs {
		if !ip.pmcs[idx].alloced {
			ret[ri] = 0
			ri++
			continue
		}
		ip.pmcs[idx].alloced = false
		c := ip._pmc_stop(idx)
		ret[ri] = c
		ri++
	}
	return ret
}

func (ip *intelprof_t) startnmi(evid pmevid_t, pf pmflag_t, min,
    max uint) bool {
	ip.l.Lock()
	defer ip.l.Unlock()
	if ip.pmcs[0].alloced {
		return false
	}
	if _, ok := ip.events[evid]; !ok {
		return false
	}
	// NMI profiling currently only uses pmc0 (but could use any other
	// counter)
	ip.pmcs[0].alloced = true

	v := ip._ev2msr(evid, pf)
	// enable LVT interrupt on PMC overflow
	inte := 1 << 20
	v |= inte

	mask := false
	runtime.SetNMI(mask, v, min, max)
	ip._enableall()
	return true
}

func (ip *intelprof_t) stopnmi() []uintptr {
	ip.l.Lock()
	defer ip.l.Unlock()

	mask := true
	runtime.SetNMI(mask, 0, 0, 0)
	ip._disableall()
	buf, full := runtime.TakeNMIBuf()
	if full {
		fmt.Printf("*** NMI buffer is full!\n")
	}

	ip.pmcs[0].alloced = false

	return buf
}


var failalloc bool = false

var failsites = make(map[uintptr]bool)
// white-listed functions; don't fail these allocations. terminate() is for
// init resurrection.
var oksites = map[string]bool{"main.main":true, "main.(*common.Proc_t).terminate":true,}

func _pchash(pcs []uintptr) uintptr {
	if len(pcs) == 0 {
		panic("d'oh")
	}
	var ret uintptr
	for _, pc := range pcs {
		pc = pc * 1103515245 + 12345
		ret ^= pc
	}
	return ret
}

// returns true if the allocation should fail
func _fakefail() bool {
	if !failalloc {
		return false
	}
	//return rand.Intn(10000) < 10
	var pcs []uintptr
	for sz, got := 30, 30; got >= sz; sz *= 2 {
		if sz != 30 {
			fmt.Printf("!")
		}
		pcs = make([]uintptr, 30)
		// get caller of _refpg_new
		got = runtime.Callers(4, pcs)
		if got == 0 {
			panic("no")
		}
	}
	h := _pchash(pcs)
	if ok := failsites[h]; !ok {
		failsites[h] = true
		frames := runtime.CallersFrames(pcs)
		fs := ""
		// check for white-listed caller
		for {
			fr, more := frames.Next()
			if ok := oksites[fr.Function]; ok {
				return false
			}
			if fs == "" {
				fs = fmt.Sprintf("%v (%v:%v)->", fr.Function,
				   fr.File, fr.Line)
			} else {
				fs += fr.Function + "->"
			}
			if !more || fr.Function == "runtime.goexit" {
				break
			}
		}
		fmt.Printf("failing: %v\n", fs)
		return true
	}
	return false
}

func callerdump() {
	i := 3
	s := ""
	for {
		_, f, l, ok := runtime.Caller(i)
		if !ok {
			break
		}
		i++
		li := strings.LastIndex(f, "/")
		if li != -1 {
			f = f[li + 1:]
		}
		if s == "" {
			s = fmt.Sprintf("%s:%d", f, l)
		} else {
			s += fmt.Sprintf("<-%s:%d", f, l)
		}
	}
	fmt.Printf("%s\n", s)
}



func phys_init() {
	// reserve 128MB of pages
	//respgs := 1 << 15
	respgs := 1 << 16
	//respgs := 1 << (31 - 12)
	// 7.5 GB
	//respgs := 1835008
	//respgs := 1 << 18 + (1 <<17)
	physmem.pgs = make([]physpg_t, respgs)
	for i := range physmem.pgs {
		physmem.pgs[i].refcnt = -10
	}
	first := common.Pa_t(runtime.Get_phys())
	fpgn := _pg2pgn(first)
	physmem.startn = fpgn
	physmem.freei = 0
	physmem.pmaps = ^uint32(0)
	physmem.pgs[0].refcnt = 0
	physmem.pgs[0].nexti = ^uint32(0)
	last := physmem.freei
	for i := 0; i < respgs - 1; i++ {
		p_pg := common.Pa_t(runtime.Get_phys())
		pgn := _pg2pgn(p_pg)
		idx := pgn - physmem.startn
		// Get_phys() may skip regions.
		if int(idx) >= len(physmem.pgs) {
			if respgs - i > int(float64(respgs)*0.01) {
				panic("got many bad pages")
			}
			break
		}
		physmem.pgs[idx].refcnt = 0
		physmem.pgs[last].nexti = idx;
		physmem.pgs[idx].nexti =  ^uint32(0)
		last = idx
	}
	fmt.Printf("Reserved %v pages (%vMB)\n", respgs, respgs >> 8)
}

func pgcount() (int, int) {
	physmem.Lock()
	r1 := 0
	for i := physmem.freei; i != ^uint32(0); i = physmem.pgs[i].nexti {
		r1++
	}
	r2 := pmapcount()
	physmem.Unlock()
	return r1, r2
}

func _pmcount(pml4 common.Pa_t, lev int) int {
	pg := pg2pmap(dmap(pml4))
	ret := 0
	for _, pte := range pg {
		if pte & PTE_U != 0 && pte & PTE_P != 0 {
			ret += 1 + _pmcount(common.Pa_t(pte & PTE_ADDR), lev - 1)
		}
	}
	return ret
}

func pmapcount() int {
	c := 0
	for ni := physmem.pmaps; ni != ^uint32(0); ni = physmem.pgs[ni].nexti {
		v := _pmcount(common.Pa_t(ni+ physmem.startn) << PGSHIFT, 4)
		c += v
	}
	return c
}

func structchk() {
	if unsafe.Sizeof(stat_t{}) != 9*8 {
		panic("bad stat_t size")
	}
}

var lhits int

func main() {
	// magic loop
	//if rand.Int() != 0 {
	//	for {
	//	}
	//}
	bsp_apic_id = lap_id()
	phys_init()

	go func() {
		<- time.After(10*time.Second)
		fmt.Printf("[It is now safe to benchmark...]\n")
	}()

	go func() {
		for {
			<- time.After(1*time.Second)
			got := lhits
			lhits = 0
			if got != 0 {
				fmt.Printf("*** limit hits: %v\n", got)
			}
		}
	}()

	fmt.Printf("              BiscuitOS\n")
	fmt.Printf("          go version: %v\n", runtime.Version())
	pmem := runtime.Totalphysmem()
	fmt.Printf("  %v MB of physical memory\n", pmem >> 20)

	structchk()
	cpuchk()
	net_init()

	dmap_init()
	perfsetup()

	// must come before any irq_unmask()s
	runtime.Install_traphandler(trapstub)

	//pci_dump()
	ncpu := attach_devs()

	kbd_init()

	// control CPUs
	aplim := 7
	cpus_start(ncpu, aplim)
	//runtime.SCenable = false

	rf := fs.MkFS()

	exec := func(cmd string, args []string) {
		fmt.Printf("start [%v %v]\n", cmd, args)
		nargs := []string{cmd}
		nargs = append(nargs, args...)
		defaultfds := []*common.Fd_t{&fd_stdin, &fd_stdout, &fd_stderr}
		p, ok := proc_new(cmd, rf, defaultfds)
		if !ok {
			panic("silly sysprocs")
		}
		var tf [common.TFSIZE]uintptr
		ret := sys_execv1(p, &tf, cmd, nargs)
		if ret != 0 {
			panic(fmt.Sprintf("exec failed %v", ret))
		}
		p.sched_add(&tf, p.tid0)
	}

	//exec("bin/lsh", nil)
	exec("bin/init", nil)
	//exec("bin/rs", []string{"/redis.conf"})

	//go func() {
	//	d := time.Second
	//	for {
	//		<- time.After(d)
	//		ms := &runtime.MemStats{}
	//		runtime.ReadMemStats(ms)
	//		fmt.Printf("%v MiB\n", ms.Alloc/ (1 << 20))
	//	}
	//}()

	// sleep forever
	var dur chan bool
	<- dur
}

func findbm() {
	dmap_init()
	//n := incn()
	//var aplim int
	//if n == 0 {
	//	aplim = 1
	//} else {
	//	aplim = 7
	//}
	al := 7
	cpus_start(al, al)

	ch := make(chan bool)
	times := uint64(0)
	sum := uint64(0)
	for {
		st0 := runtime.Rdtsc()
		go func(st uint64) {
			tot := runtime.Rdtsc() - st
			sum += tot
			times++
			if times % 1000000 == 0 {
				fmt.Printf("%9v cycles to find (avg)\n",
				    sum/times)
				sum = 0
				times = 0
			}
			ch <- true
		}(st0)
		//<- ch
		loopy: for {
			select {
			case <- ch:
				break loopy
			default:
			}
		}
	}
}
