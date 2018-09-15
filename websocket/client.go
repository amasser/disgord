package websocket

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andersfylling/disgord/websocket/opcode"
	. "github.com/andersfylling/snowflake"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

type Config struct {
	Token         string
	HTTPClient    *http.Client
	DAPIVersion   int
	DAPIEncoding  string
	Debug         bool
	ChannelBuffer uint

	// for identify packets
	Browser string
	Device  string
}

func (c *Config) Validate() (err error) {
	if c.Token == "" {
		err = errors.New("missing Config.Token for discord authentication")
		return
	}

	if c.Browser == "" {
		err = errors.New("missing Config.Browser for discord identification")
		return
	}

	if c.Device == "" {
		err = errors.New("missing Config.Device for discord identification")
		return
	}

	// ensure this socket module supports the given discord api version
	if c.DAPIVersion < LowestAPIVersion || c.DAPIVersion > HighestAPIVersion {
		err = fmt.Errorf("discord API version %d is not supported. Lowest supported version is %d, and highest is %d", c.DAPIVersion, LowestAPIVersion, HighestAPIVersion)
		return
	}

	encoding := strings.ToLower(c.DAPIEncoding)
	if c.DAPIEncoding != encoding {
		err = fmt.Errorf("communication encoding type must be lowercase. Got '%s'", c.DAPIEncoding)
		return
	}

	var acceptedEncoding bool
	for _, supported := range Encodings {
		if encoding == supported {
			acceptedEncoding = true
			break
		}
	}
	if !acceptedEncoding {
		err = fmt.Errorf("discord requires data encoding to be of the following '%s', while '%s' encoding was requested", strings.Join(Encodings, "', '"), encoding)
		return
	}

	if c.ChannelBuffer < 1 {
		err = errors.New("Config.ChannelBuffer must be at least 1 or more")
		return
	}

	return
}

// NewRequiredClient same as NewClient(...), but program exits on failure.
func NewRequiredClient(conf *Config) DiscordWebsocket {
	c, err := NewClient(conf)
	if err != nil {
		logrus.Fatal(err)
	}

	return c
}

// NewClient Creates a new discord websocket client
func NewClient(conf *Config) (DiscordWebsocket, error) {
	if conf == nil {
		return nil, errors.New("Config struct")
	}

	err := conf.Validate()
	if err != nil {
		return nil, err
	}

	// check the http client exists. Otherwise create one.
	if conf.HTTPClient == nil {
		conf.HTTPClient = &http.Client{
			Timeout: time.Second * DefaultHTTPTimeout,
		}
	}

	// configure logrus output
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
	})

	if conf.Debug {
		logrus.SetLevel(logrus.DebugLevel)
	}

	// return configured discord websocket client
	return &Client{
		conf:               conf,
		urlAPIVersion:      BaseURL + "/v" + strconv.Itoa(conf.DAPIVersion),
		lastHeartbeatAck:   time.Now(),
		disconnected:       nil,
		discordWSEventChan: make(chan DiscordWSEvent, conf.ChannelBuffer),
		operationChan:      make(chan *gatewayEvent),
		eventChans:         make(map[string]chan []byte),
		sendChan:           make(chan *gatewayPayload),
		//Myself:            &user.User{},
	}, nil
}

type Pulsater interface {
	AllowedToStartPulsating(serviceID uint8) bool
	StopPulsating(serviceID uint8)
	HeartbeatInterval() uint
	LastHeartbeatAck() time.Time
	HeartbeatWasRecieved(last time.Time) bool
	GetSocketInfo() (time.Time, uint, uint)
	PingServer(snr uint)
	HeartbeatAckMissingFix()
}

// DiscordWebsocket interface for interacting with the websocket module
// TODO: add channels / listener for failed reconnections
type DiscordWebsocket interface {
	DiscordWSEventChan() <-chan DiscordWSEvent
	Connect() (err error)
	Disconnect() (err error)
	MockEventChanReciever()
}

// Client holds the web socket state and can be used directly in marshal/unmarshal to work with intance data
type Client struct {
	sync.RWMutex `json:"-"`
	conf         *Config

	urlAPIVersion string

	// URL Websocket URL web socket url
	url            string
	sequenceNumber uint

	heartbeatInterval uint `json:"heartbeat_interval"`
	lastHeartbeatAck  time.Time
	Trace             []string  `json:"_trace"`
	SessionID         string    `json:"session_id"`
	ShardCount        uint      `json:"shard_count"`
	ShardID           Snowflake `json:"shard_id"`

	disconnected       chan struct{}
	operationChan      chan *gatewayEvent
	eventChans         map[string]chan []byte
	sendChan           chan *gatewayPayload
	discordWSEventChan chan DiscordWSEvent

	//Myself         *user.User  `json:"user"`
	//MyselfSettings interface{} `json:"user_settings"`

	// websocket connection
	conn    *websocket.Conn
	wsMutex sync.Mutex // https://hackernoon.com/dancing-with-go-s-mutexes-92407ae927bf

	// heartbeat mutex keeps us from creating another pulser
	pulseMutex sync.RWMutex
	pulsating  int
}

// MockEventChanReciever removes events from the channel such that the next
//											 event can be inserted.
func (c *Client) MockEventChanReciever() {
	go func(client *Client) {
		for {
			ok := false
			select {
			case <-client.DiscordWSEventChan():
				ok = true
			case <-client.disconnected:
			}
			if !ok {
				break
			}
		}
	}(c)
}

func (client *Client) HeartbeatInterval() uint {
	client.RLock()
	defer client.RUnlock()

	return client.heartbeatInterval
}

func (client *Client) LastHeartbeatAck() time.Time {
	client.RLock()
	defer client.RUnlock()

	return client.lastHeartbeatAck
}

func (client *Client) GetSocketInfo() (time.Time, uint, uint) {
	client.RLock()
	defer client.RUnlock()

	return client.lastHeartbeatAck, client.heartbeatInterval, client.sequenceNumber

}

func (client *Client) PingServer(snr uint) {
	client.sendChan <- &gatewayPayload{Op: opcode.Ping, Data: snr}
}

func (client *Client) HeartbeatAckMissingFix() {
	client.Disconnect()
	go client.reconnect()
}

func (client *Client) HeartbeatWasRecieved(last time.Time) bool {
	client.RLock()
	defer client.RUnlock()

	return client.lastHeartbeatAck.After(last)
}

// AllowedToStartPulsating you must notify when you are done pulsating!
func (client *Client) AllowedToStartPulsating(serviceID uint8) bool {
	client.pulseMutex.RLock()
	pulsating := client.pulsating > 0
	client.pulseMutex.RUnlock()

	client.pulseMutex.Lock()
	if pulsating {
		client.pulseMutex.Unlock()
		return false
	}

	client.pulsating = int(serviceID)
	client.pulseMutex.Unlock()

	return true
}

func (client *Client) StopPulsating(serviceID uint8) {
	client.pulseMutex.RLock()
	pulsating := client.pulsating > 0 && client.pulsating == int(serviceID)
	client.pulseMutex.RUnlock()

	client.pulseMutex.Lock()
	if pulsating {
		client.pulseMutex.Unlock()
		return
	}

	client.pulsating = -1
	client.pulseMutex.Unlock()
}

// todo: remove or rewrite
func (c *Client) String() string {
	return fmt.Sprintf("%s v%d.%d.%d", LibName, LibVersionMajor, LibVersionMinor, LibVersionPatch)
}

func (c *Client) incrementSequenceNumber() {
	c.Lock()
	c.sequenceNumber++
	c.Unlock()
}

func (c *Client) updateSession(gp *gatewayEvent) {
	ready := &readyPacket{}
	err := unmarshal(gp.Data.ByteArr(), ready)
	if err != nil {
		logrus.Error(err)
	}

	c.RLock()
	c.SessionID = ready.SessionID
	c.Trace = ready.Trace
	c.RUnlock()
}

// Dead check if the websocket connection isn't established AKA "dead"
func (c *Client) Dead() bool {
	return c.conn == nil
}

// Routed checks if the client has received the root endpoint for discord API communication
func (c *Client) Routed() bool {
	return c.url != ""
}

// RemoveRoute deletes cached discord wss endpoint
func (c *Client) RemoveRoute() {
	c.url = ""
}

func (c *Client) DiscordWSEventChan() <-chan DiscordWSEvent {
	return c.discordWSEventChan
}

func (c *Client) readPump() {
	logrus.Debug("Listening for packets...")

	for {
		messageType, packet, err := c.conn.ReadMessage()
		if err != nil {
			var die bool
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				// logrus.Errorf("error(%d): %v", messageType, err)
				die = true
			} else if c.disconnected == nil {
				// connection was closed
				die = true
			}

			if die {
				logrus.Debug("closing readPump")
				return
			}
		}

		logrus.Debugf("<-: %+v\n", string(packet))

		// TODO: Improve zlib performance
		if messageType == websocket.BinaryMessage {
			b := bytes.NewReader(packet)

			r, err := zlib.NewReader(b)
			if err != nil {
				panic(err)
			}

			buf := new(bytes.Buffer)
			buf.ReadFrom(r)
			packet = buf.Bytes()

			r.Close()
		}

		// parse to gateway payload object
		evt := &gatewayEvent{}
		err = unmarshal(packet, evt)
		if err != nil {
			logrus.Error(err)
		}

		// notify operation listeners
		c.operationChan <- evt
	}
}

func (c *Client) SequenceNumber() uint {
	c.RLock()
	defer c.RUnlock()

	return c.sequenceNumber
}

// Connect establishes a websocket connection to the discord API
// if Connect() fails, it closes channels and connections
func (c *Client) Connect() (err error) {
	c.Lock()
	defer c.Unlock()

	// check if web socket connection is already open
	if !c.Dead() {
		return errors.New("websocket connection already established, cannot open a new one")
	}

	// discord API sends a web socket url which should be used.
	// It's required to be cached, and only re-requested whenever disgord is unable
	// to reconnect to the API..
	if !c.Routed() {
		c.url, err = getGatewayRoute(c.conf.HTTPClient, c.conf.DAPIVersion, c.conf.DAPIEncoding)
		if err != nil {
			return
		}
	}

	defer func(err error) error {
		if err != nil {
			if c.conn != nil {
				c.conn.Close()
				c.conn = nil
				close(c.disconnected)
			}
			logrus.Error(err)
			return err
		}
		return nil
	}(err)

	// establish ws connection
	logrus.Debug("Connecting to url: " + c.url)
	c.conn, _, err = websocket.DefaultDialer.Dial(c.url, nil)
	if err != nil {
		return
	}
	logrus.Debug("established web socket connection")
	c.disconnected = make(chan struct{})

	// setup operation listeners
	// These handle sepecific "events" related to the socket connection
	go c.operationHandlers()

	// setup read and write goroutines
	go c.readPump()
	go c.writePump(c.conn)

	return
}

// Disconnect closes the discord websocket connection
func (c *Client) Disconnect() (err error) {
	c.RLock()
	if c.conn == nil {
		err = errors.New("no websocket connection exist")
		return
	}
	c.RUnlock()

	c.wsMutex.Lock()
	err = c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	c.wsMutex.Unlock()
	if err != nil {
		logrus.Warningln(err)
	}
	select {
	case <-c.disconnected:
		// might get closed by another process
	case <-time.After(time.Second * 1):
		close(c.disconnected)
	}

	// give remainding processes some time to exit
	<-time.After(time.Second * 1)
	c.disconnected = nil

	// close connection
	err = c.conn.Close()
	c.conn = nil
	return
}

// Reconnect to discord endpoint
func (c *Client) reconnect() (err error) {
	for try := 0; try <= MaxReconnectTries; try++ {

		logrus.Debugf("Reconnect attempt #%d\n", try)
		err = c.Connect()
		if err == nil {
			logrus.Info("successfully reconnected")

			// send resume package

			break
			// TODO voice
		}
		if try == MaxReconnectTries-1 {
			err = errors.New("Too many reconnect attempts")
			return err
		}

		// wait 5 seconds
		logrus.Info("reconnect failed, trying again in 5 seconds")
		<-time.After(time.Duration(5) * time.Second)
	}

	return
}