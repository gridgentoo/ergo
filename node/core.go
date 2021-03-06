package node

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ergo-services/ergo/etf"
	"github.com/ergo-services/ergo/gen"
	"github.com/ergo-services/ergo/lib"
)

const (
	startPID = 1000
)

type core struct {
	monitorInternal
	networkInternal

	ctx  context.Context
	stop context.CancelFunc

	env map[gen.EnvKey]interface{}

	tls TLS

	nextPID  uint64
	uniqID   uint64
	nodename string
	creation uint32

	names          map[string]etf.Pid
	mutexNames     sync.Mutex
	aliases        map[etf.Alias]*process
	mutexAliases   sync.Mutex
	processes      map[uint64]*process
	mutexProcesses sync.Mutex

	behaviors      map[string]map[string]gen.RegisteredBehavior
	mutexBehaviors sync.Mutex
}

type coreInternal interface {
	gen.Core
	CoreRouter
	monitorInternal
	networkInternal

	spawn(name string, opts processOptions, behavior gen.ProcessBehavior, args ...etf.Term) (gen.Process, error)

	registerName(name string, pid etf.Pid) error
	unregisterName(name string) error

	newAlias(p *process) (etf.Alias, error)
	deleteAlias(owner *process, alias etf.Alias) error

	coreNodeName() string
	coreStop()
	coreUptime() int64
	coreIsAlive() bool

	coreWait()
	coreWaitWithTimeout(d time.Duration) error
}

func newCore(ctx context.Context, nodename string, options Options) (coreInternal, error) {
	c := &core{
		ctx:     ctx,
		nextPID: startPID,
		uniqID:  uint64(time.Now().UnixNano()),
		// keep node to get the process to access to the node's methods
		nodename:  nodename,
		creation:  options.Creation,
		names:     make(map[string]etf.Pid),
		aliases:   make(map[etf.Alias]*process),
		processes: make(map[uint64]*process),
		behaviors: make(map[string]map[string]gen.RegisteredBehavior),
	}

	corectx, corestop := context.WithCancel(ctx)
	c.stop = corestop
	c.ctx = corectx

	c.monitorInternal = newMonitor(nodename, CoreRouter(c))
	network, err := newNetwork(c.ctx, nodename, options, CoreRouter(c))
	if err != nil {
		corestop()
		return nil, err
	}
	c.networkInternal = network
	return c, nil
}

func (c *core) coreNodeName() string {
	return c.nodename
}

func (c *core) coreStop() {
	c.stop()
	c.stopNetwork()
}

func (c *core) coreUptime() int64 {
	return time.Now().Unix() - int64(c.creation)
}

func (c *core) coreWait() {
	<-c.ctx.Done()
}

// WaitWithTimeout waits until node stopped. Return ErrTimeout
// if given timeout is exceeded
func (c *core) coreWaitWithTimeout(d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return ErrTimeout
	case <-c.ctx.Done():
		return nil
	}
}

// IsAlive returns true if node is running
func (c *core) coreIsAlive() bool {
	return c.ctx.Err() == nil
}

func (c *core) newPID() etf.Pid {
	// http://erlang.org/doc/apps/erts/erl_ext_dist.html#pid_ext
	// https://stackoverflow.com/questions/243363/can-someone-explain-the-structure-of-a-pid-in-erlang
	i := atomic.AddUint64(&c.nextPID, 1)
	return etf.Pid{
		Node:     etf.Atom(c.nodename),
		ID:       i,
		Creation: c.creation,
	}

}

// MakeRef returns atomic reference etf.Ref within this node
func (c *core) MakeRef() (ref etf.Ref) {
	ref.Node = etf.Atom(c.nodename)
	ref.Creation = c.creation
	nt := atomic.AddUint64(&c.uniqID, 1)
	ref.ID[0] = uint32(uint64(nt) & ((2 << 17) - 1))
	ref.ID[1] = uint32(uint64(nt) >> 46)
	return
}

// IsAlias
func (c *core) IsAlias(alias etf.Alias) bool {
	c.mutexAliases.Lock()
	_, ok := c.aliases[alias]
	c.mutexAliases.Unlock()
	return ok
}

func (c *core) newAlias(p *process) (etf.Alias, error) {
	var alias etf.Alias

	// chech if its alive
	c.mutexProcesses.Lock()
	_, exist := c.processes[p.self.ID]
	c.mutexProcesses.Unlock()
	if !exist {
		return alias, ErrProcessUnknown
	}

	alias = etf.Alias(c.MakeRef())
	lib.Log("[%s] CORE create process alias for %v: %s", c.nodename, p.self, alias)

	c.mutexAliases.Lock()
	c.aliases[alias] = p
	c.mutexAliases.Unlock()

	p.Lock()
	p.aliases = append(p.aliases, alias)
	p.Unlock()
	return alias, nil
}

func (c *core) deleteAlias(owner *process, alias etf.Alias) error {
	lib.Log("[%s] CORE delete process alias %v for %v", c.nodename, alias, owner.self)

	c.mutexAliases.Lock()
	p, alias_exist := c.aliases[alias]
	c.mutexAliases.Unlock()

	if alias_exist == false {
		return ErrAliasUnknown
	}

	c.mutexProcesses.Lock()
	_, process_exist := c.processes[owner.self.ID]
	c.mutexProcesses.Unlock()

	if process_exist == false {
		return ErrProcessUnknown
	}
	if p.self != owner.self {
		return ErrAliasOwner
	}

	p.Lock()
	for i := range p.aliases {
		if alias != p.aliases[i] {
			continue
		}
		// remove it from the global alias list
		c.mutexAliases.Lock()
		delete(c.aliases, alias)
		c.mutexAliases.Unlock()
		// remove it from the process alias list
		p.aliases[i] = p.aliases[0]
		p.aliases = p.aliases[1:]
		p.Unlock()
		return nil
	}
	p.Unlock()

	// shouldn't reach this code. seems we got a bug
	fmt.Println("Bug: Process lost its alias. Please, report this issue")
	c.mutexAliases.Lock()
	delete(c.aliases, alias)
	c.mutexAliases.Unlock()

	return ErrAliasUnknown
}

func (c *core) newProcess(name string, behavior gen.ProcessBehavior, opts processOptions) (*process, error) {

	var parentContext context.Context

	mailboxSize := DefaultProcessMailboxSize
	if opts.MailboxSize > 0 {
		mailboxSize = int(opts.MailboxSize)
	}

	parentContext = c.ctx

	processContext, kill := context.WithCancel(parentContext)
	if opts.Context != nil {
		processContext, _ = context.WithCancel(opts.Context)
	}

	pid := c.newPID()

	env := make(map[gen.EnvKey]interface{})
	// inherite the node environment
	for k, v := range c.env {
		env[k] = v
	}
	// merge the custom ones
	for k, v := range opts.Env {
		env[k] = v
	}

	process := &process{
		coreInternal: c,

		self:     pid,
		name:     name,
		behavior: behavior,
		env:      env,

		parent:      opts.parent,
		groupLeader: opts.GroupLeader,

		mailBox:      make(chan gen.ProcessMailboxMessage, mailboxSize),
		gracefulExit: make(chan gen.ProcessGracefulExitRequest, mailboxSize),
		direct:       make(chan gen.ProcessDirectMessage),

		context: processContext,
		kill:    kill,

		reply: make(map[etf.Ref]chan etf.Term),
	}

	process.exit = func(from etf.Pid, reason string) error {
		lib.Log("[%s] EXIT from %s to %s with reason: %s", c.nodename, from, pid, reason)
		if processContext.Err() != nil {
			// process is already died
			return ErrProcessUnknown
		}

		ex := gen.ProcessGracefulExitRequest{
			From:   from,
			Reason: reason,
		}

		// use select just in case if this process isn't been started yet
		// or ProcessLoop is already exited (has been set to nil)
		// otherwise it cause infinity lock
		select {
		case process.gracefulExit <- ex:
		default:
			return ErrProcessBusy
		}

		// let the process decide whether to stop itself, otherwise its going to be killed
		if !process.trapExit {
			process.kill()
		}
		return nil
	}

	if name != "" {
		lib.Log("[%s] CORE registering name (%s): %s", c.nodename, pid, name)
		c.mutexNames.Lock()
		if _, exist := c.names[name]; exist {
			c.mutexNames.Unlock()
			return nil, ErrTaken
		}
		c.names[name] = process.self
		c.mutexNames.Unlock()
	}

	lib.Log("[%s] CORE registering process: %s", c.nodename, pid)
	c.mutexProcesses.Lock()
	c.processes[process.self.ID] = process
	c.mutexProcesses.Unlock()

	return process, nil
}

func (c *core) deleteProcess(pid etf.Pid) {
	c.mutexProcesses.Lock()
	p, exist := c.processes[pid.ID]
	if !exist {
		c.mutexProcesses.Unlock()
		return
	}
	lib.Log("[%s] CORE unregistering process: %s", c.nodename, p.self)
	delete(c.processes, pid.ID)
	c.mutexProcesses.Unlock()

	c.mutexNames.Lock()
	if (p.name) != "" {
		lib.Log("[%s] CORE unregistering name (%s): %s", c.nodename, p.self, p.name)
		delete(c.names, p.name)
	}

	// delete names registered with this pid
	for name, pid := range c.names {
		if p.self == pid {
			delete(c.names, name)
		}
	}
	c.mutexNames.Unlock()

	c.mutexAliases.Lock()
	for alias := range c.aliases {
		delete(c.aliases, alias)
	}
	c.mutexAliases.Unlock()

	return
}

func (c *core) spawn(name string, opts processOptions, behavior gen.ProcessBehavior, args ...etf.Term) (gen.Process, error) {

	process, err := c.newProcess(name, behavior, opts)
	if err != nil {
		return nil, err
	}
	lib.Log("[%s] CORE spawn a new process %s (registered name: %q)", c.nodename, process.self, name)

	initProcess := func() (ps gen.ProcessState, err error) {
		if lib.CatchPanic() {
			defer func() {
				if rcv := recover(); rcv != nil {
					pc, fn, line, _ := runtime.Caller(2)
					fmt.Printf("Warning: initialization process failed %s[%q] %#v at %s[%s:%d]\n",
						process.self, name, rcv, runtime.FuncForPC(pc).Name(), fn, line)
					c.deleteProcess(process.self)
					err = fmt.Errorf("panic")
				}
			}()
		}

		ps, err = behavior.ProcessInit(process, args...)
		return
	}

	processState, err := initProcess()
	if err != nil {
		return nil, err
	}

	started := make(chan bool)
	defer close(started)

	cleanProcess := func(reason string) {
		// set gracefulExit to nil before we start termination handling
		process.gracefulExit = nil
		c.deleteProcess(process.self)
		// invoke cancel context to prevent memory leaks
		// and propagate context canelation
		process.Kill()
		// notify all the linked process and monitors
		c.handleTerminated(process.self, name, reason)
		// make the rest empty
		process.Lock()
		process.aliases = []etf.Alias{}

		// Do not clean self and name. Sometimes its good to know what pid
		// (and what name) was used by the dead process. (gen.Applications is using it)
		// process.name = ""
		// process.self = etf.Pid{}

		process.behavior = nil
		process.parent = nil
		process.groupLeader = nil
		process.exit = nil
		process.kill = nil
		process.mailBox = nil
		process.direct = nil
		process.env = nil
		process.reply = nil
		process.Unlock()
	}

	go func(ps gen.ProcessState) {
		if lib.CatchPanic() {
			defer func() {
				if rcv := recover(); rcv != nil {
					pc, fn, line, _ := runtime.Caller(2)
					fmt.Printf("Warning: process terminated %s[%q] %#v at %s[%s:%d]\n",
						process.self, name, rcv, runtime.FuncForPC(pc).Name(), fn, line)
					cleanProcess("panic")
				}
			}()
		}

		// start process loop
		reason := behavior.ProcessLoop(ps, started)
		// process stopped
		cleanProcess(reason)

	}(processState)

	// wait for the starting process loop
	<-started
	return process, nil
}

func (c *core) registerName(name string, pid etf.Pid) error {
	lib.Log("[%s] CORE registering name %s", c.nodename, name)
	c.mutexNames.Lock()
	defer c.mutexNames.Unlock()
	if _, ok := c.names[name]; ok {
		// already registered
		return ErrTaken
	}
	c.names[name] = pid
	return nil
}

func (c *core) unregisterName(name string) error {
	lib.Log("[%s] CORE unregistering name %s", c.nodename, name)
	c.mutexNames.Lock()
	defer c.mutexNames.Unlock()
	if _, ok := c.names[name]; ok {
		delete(c.names, name)
		return nil
	}
	return ErrNameUnknown
}

// RegisterBehavior
func (c *core) RegisterBehavior(group, name string, behavior gen.ProcessBehavior, data interface{}) error {
	lib.Log("[%s] CORE registering behavior %q in group %q ", c.nodename, name, group)
	var groupBehaviors map[string]gen.RegisteredBehavior
	var exist bool

	c.mutexBehaviors.Lock()
	defer c.mutexBehaviors.Unlock()

	groupBehaviors, exist = c.behaviors[group]
	if !exist {
		groupBehaviors = make(map[string]gen.RegisteredBehavior)
		c.behaviors[group] = groupBehaviors
	}

	_, exist = groupBehaviors[name]
	if exist {
		return ErrTaken
	}

	rb := gen.RegisteredBehavior{
		Behavior: behavior,
		Data:     data,
	}
	groupBehaviors[name] = rb
	return nil
}

// RegisteredBehavior
func (c *core) RegisteredBehavior(group, name string) (gen.RegisteredBehavior, error) {
	var groupBehaviors map[string]gen.RegisteredBehavior
	var rb gen.RegisteredBehavior
	var exist bool

	c.mutexBehaviors.Lock()
	defer c.mutexBehaviors.Unlock()

	groupBehaviors, exist = c.behaviors[group]
	if !exist {
		return rb, ErrBehaviorGroupUnknown
	}

	rb, exist = groupBehaviors[name]
	if !exist {
		return rb, ErrBehaviorUnknown
	}
	return rb, nil
}

// RegisteredBehaviorGroup
func (c *core) RegisteredBehaviorGroup(group string) []gen.RegisteredBehavior {
	var groupBehaviors map[string]gen.RegisteredBehavior
	var exist bool
	var listrb []gen.RegisteredBehavior

	c.mutexBehaviors.Lock()
	defer c.mutexBehaviors.Unlock()

	groupBehaviors, exist = c.behaviors[group]
	if !exist {
		return listrb
	}

	for _, v := range groupBehaviors {
		listrb = append(listrb, v)
	}
	return listrb
}

// UnregisterBehavior
func (c *core) UnregisterBehavior(group, name string) error {
	lib.Log("[%s] CORE unregistering behavior %s in group %s ", c.nodename, name, group)
	var groupBehaviors map[string]gen.RegisteredBehavior
	var exist bool

	c.mutexBehaviors.Lock()
	defer c.mutexBehaviors.Unlock()

	groupBehaviors, exist = c.behaviors[group]
	if !exist {
		return ErrBehaviorUnknown
	}
	delete(groupBehaviors, name)

	// remove group if its empty
	if len(groupBehaviors) == 0 {
		delete(c.behaviors, group)
	}
	return nil
}

// ProcessInfo
func (c *core) ProcessInfo(pid etf.Pid) (gen.ProcessInfo, error) {
	p := c.ProcessByPid(pid)
	if p == nil {
		return gen.ProcessInfo{}, fmt.Errorf("undefined")
	}

	return p.Info(), nil
}

// ProcessByPid
func (c *core) ProcessByPid(pid etf.Pid) gen.Process {
	c.mutexProcesses.Lock()
	defer c.mutexProcesses.Unlock()
	if p, ok := c.processes[pid.ID]; ok && p.IsAlive() {
		return p
	}

	// unknown process
	return nil
}

// ProcessByAlias
func (c *core) ProcessByAlias(alias etf.Alias) gen.Process {
	c.mutexAliases.Lock()
	defer c.mutexAliases.Unlock()
	if p, ok := c.aliases[alias]; ok && p.IsAlive() {
		return p
	}
	// unknown process
	return nil
}

// ProcessByName
func (c *core) ProcessByName(name string) gen.Process {
	var pid etf.Pid
	if name != "" {
		// requesting Process by name
		c.mutexNames.Lock()

		if p, ok := c.names[name]; ok {
			pid = p
		} else {
			c.mutexNames.Unlock()
			return nil
		}
		c.mutexNames.Unlock()
	}

	return c.ProcessByPid(pid)
}

// ProcessList
func (c *core) ProcessList() []gen.Process {
	list := []gen.Process{}
	c.mutexProcesses.Lock()
	for _, p := range c.processes {
		list = append(list, p)
	}
	c.mutexProcesses.Unlock()
	return list
}

//
// implementation of CoreRouter interface:
// RouteSend
// RouteSendReg
// RouteSendAlias
//

// RouteSend implements RouteSend method of Router interface
func (c *core) RouteSend(from etf.Pid, to etf.Pid, message etf.Term) error {
	// do not allow to send from the alien node. Proxy request must be used.
	if string(from.Node) != c.nodename {
		return ErrSenderUnknown
	}

	if string(to.Node) == c.nodename {
		if to.Creation != c.creation {
			// message is addressed to the previous incarnation of this PID
			return ErrProcessIncarnation
		}
		// local route
		c.mutexProcesses.Lock()
		p, exist := c.processes[to.ID]
		c.mutexProcesses.Unlock()
		if !exist {
			lib.Log("[%s] CORE route message by pid (local) %s failed. Unknown process", c.nodename, to)
			return ErrProcessUnknown
		}
		lib.Log("[%s] CORE route message by pid (local) %s", c.nodename, to)
		select {
		case p.mailBox <- gen.ProcessMailboxMessage{from, message}:
		default:
			return fmt.Errorf("WARNING! mailbox of %s is full. dropped message from %s", p.Self(), from)
		}
		return nil
	}

	// sending to remote node
	c.mutexProcesses.Lock()
	p_from, exist := c.processes[from.ID]
	c.mutexProcesses.Unlock()
	if !exist {
		lib.Log("[%s] CORE route message by pid (local) %s failed. Unknown sender", c.nodename, to)
		return ErrSenderUnknown
	}
	connection, err := c.GetConnection(string(to.Node))
	if err != nil {
		return err
	}

	lib.Log("[%s] CORE route message by pid (remote) %s", c.nodename, to)
	return connection.Send(p_from, to, message)
}

// RouteSendReg implements RouteSendReg method of Router interface
func (c *core) RouteSendReg(from etf.Pid, to gen.ProcessID, message etf.Term) error {
	// do not allow to send from the alien node. Proxy request must be used.
	if string(from.Node) != c.nodename {
		return ErrSenderUnknown
	}

	if to.Node == c.nodename {
		// local route
		c.mutexNames.Lock()
		pid, ok := c.names[to.Name]
		c.mutexNames.Unlock()
		if !ok {
			lib.Log("[%s] CORE route message by gen.ProcessID (local) %s failed. Unknown process", c.nodename, to)
			return ErrProcessUnknown
		}
		lib.Log("[%s] CORE route message by gen.ProcessID (local) %s", c.nodename, to)
		return c.RouteSend(from, pid, message)
	}

	// send to remote node
	c.mutexProcesses.Lock()
	p_from, exist := c.processes[from.ID]
	c.mutexProcesses.Unlock()
	if !exist {
		lib.Log("[%s] CORE route message by gen.ProcessID (local) %s failed. Unknown sender", c.nodename, to)
		return ErrSenderUnknown
	}
	connection, err := c.GetConnection(string(to.Node))
	if err != nil {
		return err
	}

	lib.Log("[%s] CORE route message by gen.ProcessID (remote) %s", c.nodename, to)
	return connection.SendReg(p_from, to, message)
}

// RouteSendAlias implements RouteSendAlias method of Router interface
func (c *core) RouteSendAlias(from etf.Pid, to etf.Alias, message etf.Term) error {
	// do not allow to send from the alien node. Proxy request must be used.
	if string(from.Node) != c.nodename {
		return ErrSenderUnknown
	}

	lib.Log("[%s] CORE route message by alias %s", c.nodename, to)
	if string(to.Node) == c.nodename {
		// local route by alias
		c.mutexAliases.Lock()
		process, ok := c.aliases[to]
		c.mutexAliases.Unlock()
		if !ok {
			lib.Log("[%s] CORE route message by alias (local) %s failed. Unknown process", c.nodename, to)
			return ErrProcessUnknown
		}
		return c.RouteSend(from, process.self, message)
	}

	// send to remote node
	c.mutexProcesses.Lock()
	p_from, exist := c.processes[from.ID]
	c.mutexProcesses.Unlock()
	if !exist {
		lib.Log("[%s] CORE route message by alias (local) %s failed. Unknown sender", c.nodename, to)
		return ErrSenderUnknown
	}
	connection, err := c.GetConnection(string(to.Node))
	if err != nil {
		return err
	}

	lib.Log("[%s] CORE route message by alias (remote) %s", c.nodename, to)
	return connection.SendAlias(p_from, to, message)
}

// RouteProxy
func (c *core) RouteProxy() error {
	// FIXME
	return nil
}

// RouteSpawnRequest
func (c *core) RouteSpawnRequest(behaviorName string, request gen.RemoteSpawnRequest) (etf.Pid, error) {
	//rb, err_behavior := n.registrar.RegisteredBehavior(remoteBehaviorGroup, string(module))
	// FIXME
	//process_opts := processOptions{}
	//process_opts.Env = map[string]interface{}{"ergo:RemoteSpawnRequest": remote_request}

	//process, err_spawn := n.registrar.spawn(request.Name, process_opts, rb.Behavior, args...)
	return etf.Pid{}, nil
}

// RouteSpawnReply
func (c *core) RouteSpawnReply(to etf.Pid, ref etf.Ref, result etf.Term) error {
	// FIXME
	//			process := n.registrar.ProcessByPid(to)
	//			if process == nil {
	//				return
	//			}
	//			//flags := t.Element(4)
	//			process.PutSyncReply(ref, t.Element(5))
	return nil
}
