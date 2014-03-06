package irc

import (
	"bufio"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strings"
	"syscall"
	"time"
)

type Server struct {
	channels  ChannelNameMap
	clients   *ClientLookupSet
	commands  chan Command
	ctime     time.Time
	db        *sql.DB
	idle      chan *Client
	motdFile  string
	name      string
	newConns  chan net.Conn
	operators map[string][]byte
	password  []byte
	signals   chan os.Signal
	timeout   chan *Client
}

func NewServer(config *Config) *Server {
	server := &Server{
		channels:  make(ChannelNameMap),
		clients:   NewClientLookupSet(),
		commands:  make(chan Command, 16),
		ctime:     time.Now(),
		db:        OpenDB(config.Server.Database),
		idle:      make(chan *Client, 16),
		motdFile:  config.Server.MOTD,
		name:      config.Server.Name,
		newConns:  make(chan net.Conn, 16),
		operators: config.Operators(),
		signals:   make(chan os.Signal, 1),
		timeout:   make(chan *Client, 16),
	}

	if config.Server.Password != "" {
		server.password = config.Server.PasswordBytes()
	}

	server.loadChannels()

	for _, addr := range config.Server.Listen {
		go server.listen(addr)
	}

	signal.Notify(server.signals, syscall.SIGINT, syscall.SIGHUP,
		syscall.SIGTERM, syscall.SIGQUIT)

	return server
}

func (server *Server) loadChannels() {
	rows, err := server.db.Query(`
        SELECT name, flags, key, topic, user_limit
          FROM channel`)
	if err != nil {
		log.Fatal("error loading channels: ", err)
	}
	for rows.Next() {
		var name, flags, key, topic string
		var userLimit uint64
		err = rows.Scan(&name, &flags, &key, &topic, &userLimit)
		if err != nil {
			log.Println(err)
			continue
		}

		channel := NewChannel(server, name)
		for _, flag := range flags {
			channel.flags[ChannelMode(flag)] = true
		}
		channel.key = key
		channel.topic = topic
		channel.userLimit = userLimit
	}
}

func (server *Server) processCommand(cmd Command) {
	client := cmd.Client()
	if DEBUG_SERVER {
		log.Printf("%s → %s %s", client, server, cmd)
	}

	switch client.phase {
	case Authorization:
		authCmd, ok := cmd.(AuthServerCommand)
		if !ok {
			client.Quit("unexpected command")
			return
		}
		authCmd.HandleAuthServer(server)

	case Registration:
		regCmd, ok := cmd.(RegServerCommand)
		if !ok {
			client.Quit("unexpected command")
			return
		}
		regCmd.HandleRegServer(server)

	default:
		srvCmd, ok := cmd.(ServerCommand)
		if !ok {
			client.ErrUnknownCommand(cmd.Code())
			return
		}
		switch srvCmd.(type) {
		case *PingCommand, *PongCommand:
			client.Touch()

		case *QuitCommand:
			// no-op

		default:
			client.Active()
			client.Touch()
		}
		srvCmd.HandleServer(server)
	}
}

func (server *Server) Shutdown() {
	server.db.Close()
	for _, client := range server.clients.byNick {
		client.Reply(RplNotice(server, client, "shutting down"))
	}
}

func (server *Server) Run() {
	done := false
	for !done {
		select {
		case <-server.signals:
			server.Shutdown()
			done = true

		case conn := <-server.newConns:
			NewClient(server, conn)

		case cmd := <-server.commands:
			server.processCommand(cmd)

		case client := <-server.idle:
			client.Idle()

		case client := <-server.timeout:
			client.Quit("connection timeout")
		}
	}
}

func (server *Server) InitPhase() Phase {
	if server.password == nil {
		return Registration
	}
	return Authorization
}

//
// listen goroutine
//

func (s *Server) listen(addr string) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(s, "listen error: ", err)
	}

	if DEBUG_SERVER {
		log.Printf("%s listening on %s", s, addr)
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			if DEBUG_SERVER {
				log.Printf("%s accept error: %s", s, err)
			}
			continue
		}
		if DEBUG_SERVER {
			log.Printf("%s accept: %s", s, conn.RemoteAddr())
		}

		s.newConns <- conn
	}
}

//
// server functionality
//

func (s *Server) tryRegister(c *Client) {
	if c.HasNick() && c.HasUsername() {
		c.Register()
		c.RplWelcome()
		c.RplYourHost()
		c.RplCreated()
		c.RplMyInfo()
		s.MOTD(c)
	}
}

func (server *Server) MOTD(client *Client) {
	if server.motdFile == "" {
		client.ErrNoMOTD()
		return
	}

	file, err := os.Open(server.motdFile)
	if err != nil {
		client.ErrNoMOTD()
		return
	}
	defer file.Close()

	client.RplMOTDStart()
	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")

		if len(line) > 80 {
			for len(line) > 80 {
				client.RplMOTD(line[0:80])
				line = line[80:]
			}
			if len(line) > 0 {
				client.RplMOTD(line)
			}
		} else {
			client.RplMOTD(line)
		}
	}
	client.RplMOTDEnd()
}

func (s *Server) Id() string {
	return s.name
}

func (s *Server) String() string {
	return s.name
}

func (s *Server) Nick() string {
	return s.Id()
}

//
// authorization commands
//

func (msg *ProxyCommand) HandleAuthServer(server *Server) {
	msg.Client().hostname = msg.hostname
}

func (msg *CapCommand) HandleAuthServer(server *Server) {
	// TODO
}

func (msg *PassCommand) HandleAuthServer(server *Server) {
	client := msg.Client()
	if msg.err != nil {
		client.ErrPasswdMismatch()
		client.Quit("bad password")
		return
	}

	client.phase = Registration
}

func (msg *QuitCommand) HandleAuthServer(server *Server) {
	msg.Client().Quit(msg.message)
}

//
// registration commands
//

func (msg *ProxyCommand) HandleRegServer(server *Server) {
	msg.Client().hostname = msg.hostname
}

func (msg *CapCommand) HandleRegServer(server *Server) {
	// TODO
}

func (m *NickCommand) HandleRegServer(s *Server) {
	client := m.Client()

	if m.nickname == "" {
		client.ErrNoNicknameGiven()
		return
	}

	if s.clients.Get(m.nickname) != nil {
		client.ErrNickNameInUse(m.nickname)
		return
	}

	if !IsNickname(m.nickname) {
		client.ErrErroneusNickname(m.nickname)
		return
	}

	client.SetNickname(m.nickname)
	s.tryRegister(client)
}

func (msg *RFC1459UserCommand) HandleRegServer(server *Server) {
	msg.HandleRegServer2(server)
}

func (msg *RFC2812UserCommand) HandleRegServer(server *Server) {
	client := msg.Client()
	flags := msg.Flags()
	if len(flags) > 0 {
		for _, mode := range msg.Flags() {
			client.flags[mode] = true
		}
		client.RplUModeIs(client)
	}
	msg.HandleRegServer2(server)
}

func (msg *UserCommand) HandleRegServer2(server *Server) {
	client := msg.Client()
	client.username, client.realname = msg.username, msg.realname
	server.tryRegister(client)
}

func (msg *QuitCommand) HandleRegServer(server *Server) {
	msg.Client().Quit(msg.message)
}

//
// normal commands
//

func (m *PassCommand) HandleServer(s *Server) {
	m.Client().ErrAlreadyRegistered()
}

func (m *PingCommand) HandleServer(s *Server) {
	m.Client().Reply(RplPong(m.Client()))
}

func (m *PongCommand) HandleServer(s *Server) {
	// no-op
}

func (msg *NickCommand) HandleServer(server *Server) {
	client := msg.Client()

	if msg.nickname == "" {
		client.ErrNoNicknameGiven()
		return
	}

	if !IsNickname(msg.nickname) {
		client.ErrErroneusNickname(msg.nickname)
		return
	}

	if msg.nickname == client.nick {
		return
	}

	target := server.clients.Get(msg.nickname)
	if (target != nil) && (target != client) {
		client.ErrNickNameInUse(msg.nickname)
		return
	}

	client.ChangeNickname(msg.nickname)
}

func (m *UserCommand) HandleServer(s *Server) {
	m.Client().ErrAlreadyRegistered()
}

func (msg *QuitCommand) HandleServer(server *Server) {
	msg.Client().Quit(msg.message)
}

func (m *JoinCommand) HandleServer(s *Server) {
	client := m.Client()

	if m.zero {
		for channel := range client.channels {
			channel.Part(client, client.Nick())
		}
		return
	}

	for name, key := range m.channels {
		if !IsChannel(name) {
			client.ErrNoSuchChannel(name)
			continue
		}

		channel := s.channels.Get(name)
		if channel == nil {
			channel = NewChannel(s, name)
		}
		channel.Join(client, key)
	}
}

func (m *PartCommand) HandleServer(server *Server) {
	client := m.Client()
	for _, chname := range m.channels {
		channel := server.channels.Get(chname)

		if channel == nil {
			m.Client().ErrNoSuchChannel(chname)
			continue
		}

		channel.Part(client, m.Message())
	}
}

func (msg *TopicCommand) HandleServer(server *Server) {
	client := msg.Client()
	channel := server.channels.Get(msg.channel)
	if channel == nil {
		client.ErrNoSuchChannel(msg.channel)
		return
	}

	if msg.setTopic {
		channel.SetTopic(client, msg.topic)
	} else {
		channel.GetTopic(client)
	}
}

func (msg *PrivMsgCommand) HandleServer(server *Server) {
	client := msg.Client()
	if IsChannel(msg.target) {
		channel := server.channels.Get(msg.target)
		if channel == nil {
			client.ErrNoSuchChannel(msg.target)
			return
		}

		channel.PrivMsg(client, msg.message)
		return
	}

	target := server.clients.Get(msg.target)
	if target == nil {
		client.ErrNoSuchNick(msg.target)
		return
	}
	target.Reply(RplPrivMsg(client, target, msg.message))
	if target.flags[Away] {
		client.RplAway(target)
	}
}

func (m *ModeCommand) HandleServer(s *Server) {
	client := m.Client()
	target := s.clients.Get(m.nickname)

	if target == nil {
		client.ErrNoSuchNick(m.nickname)
		return
	}

	if client != target && !client.flags[Operator] {
		client.ErrUsersDontMatch()
		return
	}

	changes := make(ModeChanges, 0)

	for _, change := range m.changes {
		switch change.mode {
		case Invisible, ServerNotice, WallOps:
			switch change.op {
			case Add:
				if target.flags[change.mode] {
					continue
				}
				target.flags[change.mode] = true
				changes = append(changes, change)

			case Remove:
				if !target.flags[change.mode] {
					continue
				}
				delete(target.flags, change.mode)
				changes = append(changes, change)
			}

		case Operator, LocalOperator:
			if change.op == Remove {
				if !target.flags[change.mode] {
					continue
				}
				delete(target.flags, change.mode)
				changes = append(changes, change)
			}
		}
	}

	// Who should get these replies?
	if len(changes) > 0 {
		client.Reply(RplMode(client, target, changes))
	}
}

func (client *Client) WhoisChannelsNames() []string {
	chstrs := make([]string, len(client.channels))
	index := 0
	for channel := range client.channels {
		switch {
		case channel.members[client][ChannelOperator]:
			chstrs[index] = "@" + channel.name

		case channel.members[client][Voice]:
			chstrs[index] = "+" + channel.name

		default:
			chstrs[index] = channel.name
		}
		index += 1
	}
	return chstrs
}

func (m *WhoisCommand) HandleServer(server *Server) {
	client := m.Client()

	// TODO implement target query

	for _, mask := range m.masks {
		matches := server.clients.FindAll(mask)
		if len(matches) == 0 {
			client.ErrNoSuchNick(mask)
			continue
		}
		for mclient := range matches {
			client.RplWhois(mclient)
		}
	}
}

func (msg *ChannelModeCommand) HandleServer(server *Server) {
	client := msg.Client()
	channel := server.channels.Get(msg.channel)
	if channel == nil {
		client.ErrNoSuchChannel(msg.channel)
		return
	}

	channel.Mode(client, msg.changes)
}

func whoChannel(client *Client, channel *Channel, friends ClientSet) {
	for member := range channel.members {
		if !client.flags[Invisible] || friends[client] {
			client.RplWhoReply(channel, member)
		}
	}
}

func (msg *WhoCommand) HandleServer(server *Server) {
	client := msg.Client()
	friends := client.Friends()
	mask := msg.mask

	if mask == "" {
		for _, channel := range server.channels {
			whoChannel(client, channel, friends)
		}
	} else if IsChannel(mask) {
		// TODO implement wildcard matching
		channel := server.channels.Get(mask)
		if channel != nil {
			whoChannel(client, channel, friends)
		}
	} else {
		for mclient := range server.clients.FindAll(mask) {
			client.RplWhoReply(nil, mclient)
		}
	}

	client.RplEndOfWho(mask)
}

func (msg *OperCommand) HandleServer(server *Server) {
	client := msg.Client()

	if (msg.hash == nil) || (msg.err != nil) {
		client.ErrPasswdMismatch()
		return
	}

	client.flags[Operator] = true
	client.RplYoureOper()
	client.RplUModeIs(client)
}

func (msg *AwayCommand) HandleServer(server *Server) {
	client := msg.Client()
	if msg.away {
		client.flags[Away] = true
	} else {
		delete(client.flags, Away)
	}
	client.awayMessage = msg.text

	if client.flags[Away] {
		client.RplNowAway()
	} else {
		client.RplUnAway()
	}
}

func (msg *IsOnCommand) HandleServer(server *Server) {
	client := msg.Client()

	ison := make([]string, 0)
	for _, nick := range msg.nicks {
		if iclient := server.clients.Get(nick); iclient != nil {
			ison = append(ison, iclient.Nick())
		}
	}

	client.RplIsOn(ison)
}

func (msg *MOTDCommand) HandleServer(server *Server) {
	server.MOTD(msg.Client())
}

func (msg *NoticeCommand) HandleServer(server *Server) {
	client := msg.Client()
	if IsChannel(msg.target) {
		channel := server.channels.Get(msg.target)
		if channel == nil {
			client.ErrNoSuchChannel(msg.target)
			return
		}

		channel.Notice(client, msg.message)
		return
	}

	target := server.clients.Get(msg.target)
	if target == nil {
		client.ErrNoSuchNick(msg.target)
		return
	}
	target.Reply(RplNotice(client, target, msg.message))
}

func (msg *KickCommand) HandleServer(server *Server) {
	client := msg.Client()
	for chname, nickname := range msg.kicks {
		channel := server.channels.Get(chname)
		if channel == nil {
			client.ErrNoSuchChannel(chname)
			continue
		}

		target := server.clients.Get(nickname)
		if target == nil {
			client.ErrNoSuchNick(nickname)
			continue
		}

		channel.Kick(client, target, msg.Comment())
	}
}

func (msg *ListCommand) HandleServer(server *Server) {
	client := msg.Client()

	// TODO target server
	if msg.target != "" {
		client.ErrNoSuchServer(msg.target)
		return
	}

	if len(msg.channels) == 0 {
		for _, channel := range server.channels {
			if !client.flags[Operator] && channel.flags[Private] {
				continue
			}
			client.RplList(channel)
		}
	} else {
		for _, chname := range msg.channels {
			channel := server.channels.Get(chname)
			if channel == nil || (!client.flags[Operator] && channel.flags[Private]) {
				client.ErrNoSuchChannel(chname)
				continue
			}
			client.RplList(channel)
		}
	}
	client.RplListEnd(server)
}

func (msg *NamesCommand) HandleServer(server *Server) {
	client := msg.Client()
	if len(server.channels) == 0 {
		for _, channel := range server.channels {
			channel.Names(client)
		}
		return
	}

	for _, chname := range msg.channels {
		channel := server.channels.Get(chname)
		if channel == nil {
			client.ErrNoSuchChannel(chname)
			continue
		}
		channel.Names(client)
	}
}

func (server *Server) Reply(target *Client, format string, args ...interface{}) {
	target.Reply(RplPrivMsg(server, target, fmt.Sprintf(format, args...)))
}

func (msg *DebugCommand) HandleServer(server *Server) {
	client := msg.Client()
	if !client.flags[Operator] {
		return
	}

	switch msg.subCommand {
	case "GC":
		runtime.GC()
		server.Reply(client, "OK")

	case "GCSTATS":
		stats := &debug.GCStats{
			PauseQuantiles: make([]time.Duration, 5),
		}
		server.Reply(client, "last GC:     %s", stats.LastGC.Format(time.RFC1123))
		server.Reply(client, "num GC:      %d", stats.NumGC)
		server.Reply(client, "pause total: %s", stats.PauseTotal)
		server.Reply(client, "pause quantiles min%%: %s", stats.PauseQuantiles[0])
		server.Reply(client, "pause quantiles 25%%:  %s", stats.PauseQuantiles[1])
		server.Reply(client, "pause quantiles 50%%:  %s", stats.PauseQuantiles[2])
		server.Reply(client, "pause quantiles 75%%:  %s", stats.PauseQuantiles[3])
		server.Reply(client, "pause quantiles max%%: %s", stats.PauseQuantiles[4])

	case "NUMGOROUTINE":
		count := runtime.NumGoroutine()
		server.Reply(client, "num goroutines: %d", count)

	case "PROFILEHEAP":
		file, err := os.Create("ergonomadic.heap.prof")
		if err != nil {
			log.Printf("error: %s", err)
			break
		}
		defer file.Close()
		pprof.Lookup("heap").WriteTo(file, 0)
		server.Reply(client, "written to ergonomadic-heap.prof")
	}
}

func (msg *VersionCommand) HandleServer(server *Server) {
	client := msg.Client()
	if (msg.target != "") && (msg.target != server.name) {
		client.ErrNoSuchServer(msg.target)
		return
	}

	client.RplVersion()
}

func (msg *InviteCommand) HandleServer(server *Server) {
	client := msg.Client()

	target := server.clients.Get(msg.nickname)
	if target == nil {
		client.ErrNoSuchNick(msg.nickname)
		return
	}

	channel := server.channels.Get(msg.channel)
	if channel == nil {
		client.RplInviting(target, msg.channel)
		target.Reply(RplInviteMsg(client, target, msg.channel))
		return
	}

	channel.Invite(target, client)
}

func (msg *TimeCommand) HandleServer(server *Server) {
	client := msg.Client()
	if (msg.target != "") && (msg.target != server.name) {
		client.ErrNoSuchServer(msg.target)
		return
	}
	client.RplTime()
}

func (msg *KillCommand) HandleServer(server *Server) {
	client := msg.Client()
	if !client.flags[Operator] {
		client.ErrNoPrivileges()
		return
	}

	target := server.clients.Get(msg.nickname)
	if target == nil {
		client.ErrNoSuchNick(msg.nickname)
		return
	}

	quitMsg := fmt.Sprintf("KILLed by %s: %s", client.Nick(), msg.comment)
	target.Quit(quitMsg)
}

func (msg *WhoWasCommand) HandleServer(server *Server) {
	client := msg.Client()
	for _, nickname := range msg.nicknames {
		// TODO implement nick history
		client.ErrWasNoSuchNick(nickname)
		client.RplEndOfWhoWas(nickname)
	}
}

//
// keeping track of clients
//

type ClientLookupSet struct {
	byNick map[string]*Client
	db     *ClientDB
}

func NewClientLookupSet() *ClientLookupSet {
	return &ClientLookupSet{
		byNick: make(map[string]*Client),
		db:     NewClientDB(),
	}
}

var (
	ErrNickMissing      = errors.New("nick missing")
	ErrNicknameInUse    = errors.New("nickname in use")
	ErrNicknameMismatch = errors.New("nickname mismatch")
)

func (clients *ClientLookupSet) Get(nick string) *Client {
	return clients.byNick[strings.ToLower(nick)]
}

func (clients *ClientLookupSet) Add(client *Client) error {
	if !client.HasNick() {
		return ErrNickMissing
	}
	if clients.Get(client.nick) != nil {
		return ErrNicknameInUse
	}
	clients.byNick[strings.ToLower(client.nick)] = client
	clients.db.Add(client)
	return nil
}

func (clients *ClientLookupSet) Remove(client *Client) error {
	if !client.HasNick() {
		return ErrNickMissing
	}
	if clients.Get(client.nick) != client {
		return ErrNicknameMismatch
	}
	delete(clients.byNick, strings.ToLower(client.nick))
	clients.db.Remove(client)
	return nil
}

func ExpandUserHost(userhost string) (expanded string) {
	expanded = userhost
	// fill in missing wildcards for nicks
	if !strings.Contains(expanded, "!") {
		expanded += "!*"
	}
	if !strings.Contains(expanded, "@") {
		expanded += "@*"
	}
	return
}

func (clients *ClientLookupSet) FindAll(userhost string) (set ClientSet) {
	userhost = ExpandUserHost(userhost)
	set = make(ClientSet)
	rows, err := clients.db.db.Query(
		`SELECT nickname FROM client
           WHERE userhost LIKE ? ESCAPE '\'`,
		QuoteLike(userhost))
	if err != nil {
		return
	}
	for rows.Next() {
		var nickname string
		err := rows.Scan(&nickname)
		if err != nil {
			return
		}
		client := clients.Get(nickname)
		if client != nil {
			set.Add(client)
		}
	}
	return
}

func (clients *ClientLookupSet) Find(userhost string) *Client {
	userhost = ExpandUserHost(userhost)
	row := clients.db.db.QueryRow(
		`SELECT nickname FROM client
           WHERE userhost LIKE ? ESCAPE \
           LIMIT 1`,
		QuoteLike(userhost))
	var nickname string
	err := row.Scan(&nickname)
	if err != nil {
		log.Println("ClientLookupSet.Find: ", err)
		return nil
	}
	return clients.Get(nickname)
}

//
// client db
//

type ClientDB struct {
	db *sql.DB
}

func NewClientDB() *ClientDB {
	db := &ClientDB{
		db: OpenDB(":memory:"),
	}
	_, err := db.db.Exec(`
        CREATE TABLE client (
          nickname TEXT NOT NULL UNIQUE,
          userhost TEXT NOT NULL)`)
	if err != nil {
		log.Fatal(err)
	}
	_, err = db.db.Exec(`
        CREATE UNIQUE INDEX nickname_index ON client (nickname)`)
	if err != nil {
		log.Fatal(err)
	}
	return db
}

func (db *ClientDB) Add(client *Client) {
	_, err := db.db.Exec(`INSERT INTO client (nickname, userhost) VALUES (?, ?)`,
		client.Nick(), client.UserHost())
	if err != nil {
		log.Println(err)
	}
}

func (db *ClientDB) Remove(client *Client) {
	_, err := db.db.Exec(`DELETE FROM client WHERE nickname = ?`,
		client.Nick())
	if err != nil {
		log.Println(err)
	}
}

func QuoteLike(userhost string) (like string) {
	like = userhost
	// escape escape char
	like = strings.Replace(like, `\`, `\\`, -1)
	// escape meta-many
	like = strings.Replace(like, `%`, `\%`, -1)
	// escape meta-one
	like = strings.Replace(like, `_`, `\_`, -1)
	// swap meta-many
	like = strings.Replace(like, `*`, `%`, -1)
	// swap meta-one
	like = strings.Replace(like, `?`, `_`, -1)
	return
}
