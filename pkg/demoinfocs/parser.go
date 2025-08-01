package demoinfocs

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/golang/geo/r3"
	dp "github.com/markus-wa/godispatch"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"

	bit "github.com/markus-wa/demoinfocs-golang/v5/internal/bitread"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/common"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/cstv"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/events"
	"github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/msg"
	st "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/sendtables"
)

//go:generate ifacemaker -f parser.go -f parsing.go -s parser -i Parser -p demoinfocs -D -y "Parser is an auto-generated interface for Parser, intended to be used when mockability is needed." -c "DO NOT EDIT: Auto generated" -o parser_interface.go

type sendTableParser interface {
	ReadEnterPVS(r *bit.BitReader, index int, entities map[int]st.Entity, slot int) st.Entity
	ServerClasses() st.ServerClasses
	ParsePacket(b []byte) error
	SetInstanceBaseline(classID int, data []byte)
	OnDemoClassInfo(m *msg.CDemoClassInfo) error
	OnServerInfo(m *msg.CSVCMsg_ServerInfo) error
	OnPacketEntities(m *msg.CSVCMsg_PacketEntities) error
	OnEntity(h st.EntityHandler)
}

// header contains information from a demo's header.
type header struct {
	Filestamp       string        // aka. File-type, must be HL2DEMO
	NetworkProtocol int           // Not sure what this is for
	ServerName      string        // Server's 'hostname' config value
	ClientName      string        // Usually 'GOTV Demo'
	MapName         string        // E.g. de_cache, de_nuke, cs_office, etc.
	GameDirectory   string        // Usually 'csgo'
	PlaybackTime    time.Duration // Demo duration in seconds (= PlaybackTicks / Server's tickrate)
	PlaybackTicks   int           // Game duration in ticks (= PlaybackTime * Server's tickrate)
	PlaybackFrames  int           // Amount of 'frames' aka demo-ticks recorded (= PlaybackTime * Demo's recording rate)
}

// FrameRate returns the frame rate of the demo (frames / demo-ticks per second).
// Not necessarily the tick-rate the server ran on during the game.
//
// Returns 0 if PlaybackTime or PlaybackFrames are 0 (corrupt demo headers).
func (h *header) FrameRate() float64 {
	if h.PlaybackTime == 0 {
		return 0
	}

	return float64(h.PlaybackFrames) / h.PlaybackTime.Seconds()
}

// FrameTime returns the time a frame / demo-tick takes in seconds.
//
// Returns 0 if PlaybackTime or PlaybackFrames are 0 (corrupt demo headers).
func (h *header) FrameTime() time.Duration {
	if h.PlaybackFrames == 0 {
		return 0
	}

	return time.Duration(h.PlaybackTime.Nanoseconds() / int64(h.PlaybackFrames))
}

/*
Parser can parse a CS:GO demo.
Creating a new instance is done via NewParser().

To start off you may use Parser.ParseHeader() to parse the demo header
(this can be skipped and will be done automatically if necessary).
Further, Parser.ParseNextFrame() and Parser.ParseToEnd() can be used to parse the demo.

Use Parser.RegisterEventHandler() to receive notifications about events.

Example (without error handling):

	f, _ := os.Open("/path/to/demo.dem")
	p := dem.NewParser(f)
	defer p.Close()
	p.RegisterEventHandler(func(e events.BombExplode) {
		fmt.Printf(e.Site, "went BOOM!")
	})
	p.ParseToEnd()

Prints out '{A/B} site went BOOM!' when a bomb explodes.
*/
type parser struct {
	// Important fields

	config                          ParserConfig
	bitReader                       *bit.BitReader
	stParser                        sendTableParser
	additionalNetMessageCreators    map[int]NetMessageCreator // Map of net-message-IDs to NetMessageCreators (for parsing custom net-messages)
	msgQueue                        chan any                  // Queue of net-messages
	msgDispatcher                   *dp.Dispatcher            // Net-message dispatcher
	gameEventHandler                gameEventHandler
	eventDispatcher                 *dp.Dispatcher
	currentFrame                    int     // Demo-frame, not ingame-tick
	tickInterval                    float32 // Duration between ticks in seconds
	header                          *header // Pointer so we can check for nil
	gameState                       *gameState
	demoInfoProvider                demoInfoProvider // Provides demo infos to other packages that the core package depends on
	err                             error            // Contains a error that occurred during parsing if any
	errLock                         sync.Mutex       // Used to sync up error mutations between parsing & handling go-routines
	source2FallbackGameEventListBin []byte           // sv_hibernate_when_empty bug workaround
	ignorePacketEntitiesPanic       bool             // Used to ignore PacketEntities parsing panics (some POV demos seem to have broken rare broken PacketEntities)
	/**
	 * Set to the client slot of the recording player.
	 * Always -1 for GOTV demos.
	 */
	recordingPlayerSlot           int
	disableMimicSource1GameEvents bool

	// Additional fields, mainly caching & tracking things

	bombsiteA             bombsite
	bombsiteB             bombsite
	equipmentMapping      map[st.ServerClass]common.EquipmentType                  // Maps server classes to equipment-types
	rawPlayers            map[int]*common.PlayerInfo                               // Maps entity IDs to 'raw' player info
	modelPreCache         []string                                                 // Used to find out whether a weapon is a p250 or cz for example (same id) for Source 1 demos only
	triggers              map[int]*boundingBoxInformation                          // Maps entity IDs to triggers (used for bombsites)
	gameEventDescs        map[int32]*msg.CMsgSource1LegacyGameEventListDescriptorT // Maps game-event IDs to descriptors
	grenadeModelIndices   map[int]common.EquipmentType                             // Used to map model indices to grenades (used for grenade projectiles)
	equipmentTypePerModel map[uint64]common.EquipmentType                          // Used to retrieve the EquipmentType of grenade projectiles based on models value. Source 2 only.
	stringTables          []*msg.CSVCMsg_CreateStringTable                         // Contains all created sendtables, needed when updating them
	delayedEventHandlers  []func()                                                 // Contains event handlers that need to be executed at the end of a tick (e.g. flash events because FlashDuration isn't updated before that)
	pendingMessagesCache  []pendingMessage                                         // Cache for pending messages that need to be dispatched after the current tick
}

// NetMessageCreator creates additional net-messages to be dispatched to net-message handlers.
//
// See also: ParserConfig.AdditionalNetMessageCreators & Parser.RegisterNetMessageHandler()
type NetMessageCreator func() proto.Message

type bombsite struct {
	index  int
	center r3.Vector
}

type boundingBoxInformation struct {
	min r3.Vector
	max r3.Vector
}

func (bbi boundingBoxInformation) contains(point r3.Vector) bool {
	return point.X >= bbi.min.X && point.X <= bbi.max.X &&
		point.Y >= bbi.min.Y && point.Y <= bbi.max.Y &&
		point.Z >= bbi.min.Z && point.Z <= bbi.max.Z
}

// ServerClasses returns the server-classes of this demo.
// These are available after events.DataTablesParsed has been fired.
func (p *parser) ServerClasses() st.ServerClasses {
	return p.stParser.ServerClasses()
}

// GameState returns the current game-state.
// It contains most of the relevant information about the game such as players, teams, scores, grenades etc.
func (p *parser) GameState() GameState {
	return p.gameState
}

// CurrentFrame return the number of the current frame, aka. 'demo-tick' (Since demos often have a different tick-rate than the game).
// Starts with frame 0, should go up to header.PlaybackFrames but might not be the case (usually it's just close to it).
func (p *parser) CurrentFrame() int {
	return p.currentFrame
}

// CurrentTime returns the time elapsed since the start of the demo
func (p *parser) CurrentTime() time.Duration {
	return time.Duration(float32(p.gameState.ingameTick) * p.tickInterval * float32(time.Second))
}

// TickRate returns the tick-rate the server ran on during the game.
//
// Returns tick rate based on CSVCMsg_ServerInfo if possible.
// Otherwise returns tick rate based on demo header or -1 if the header info isn't available.
func (p *parser) TickRate() float64 {
	if p.tickInterval != 0 {
		return 1.0 / float64(p.tickInterval)
	}

	if p.header != nil {
		return legacyTickRate(*p.header)
	}

	return -1
}

func legacyTickRate(h header) float64 {
	if h.PlaybackTime == 0 {
		return 0
	}

	return float64(h.PlaybackTicks) / h.PlaybackTime.Seconds()
}

// TickTime returns the time a single tick takes in seconds.
//
// Returns tick time based on CSVCMsg_ServerInfo if possible.
// Otherwise returns tick time based on demo header or -1 if the header info isn't available.
func (p *parser) TickTime() time.Duration {
	if p.tickInterval != 0 {
		return time.Duration(float32(time.Second) * p.tickInterval)
	}

	if p.header != nil {
		return legayTickTime(*p.header)
	}

	return -1
}

func legayTickTime(h header) time.Duration {
	if h.PlaybackTicks == 0 {
		return 0
	}

	return time.Duration(h.PlaybackTime.Nanoseconds() / int64(h.PlaybackTicks))
}

// Progress returns the parsing progress from 0 to 1.
// Where 0 means nothing has been parsed yet and 1 means the demo has been parsed to the end.
//
// Might not be 100% correct since it's just based on the reported tick count of the header.
// May always return 0 if the demo header is corrupt.
func (p *parser) Progress() float32 {
	if p.header == nil || p.header.PlaybackFrames == 0 {
		return 0
	}

	return float32(p.currentFrame) / float32(p.header.PlaybackFrames)
}

/*
RegisterEventHandler registers a handler for game events.

The handler must be of type func(<EventType>) where EventType is the kind of event to be handled.
To catch all events func(any) can be used.

Example:

	parser.RegisterEventHandler(func(e events.WeaponFired) {
		fmt.Printf("%s fired his %s\n", e.Shooter.Name, e.Weapon.Type)
	})

Parameter handler has to be of type any because Go generics only work on functions, not methods.

Returns an identifier with which the handler can be removed via UnregisterEventHandler().
*/
func (p *parser) RegisterEventHandler(handler any) dp.HandlerIdentifier {
	return p.eventDispatcher.RegisterHandler(handler)
}

// UnregisterEventHandler removes a game event handler via identifier.
//
// The identifier is returned at registration by RegisterEventHandler().
func (p *parser) UnregisterEventHandler(identifier dp.HandlerIdentifier) {
	p.eventDispatcher.UnregisterHandler(identifier)
}

/*
RegisterNetMessageHandler registers a handler for net-messages.

The handler must be of type func(*<MessageType>) where MessageType is the kind of net-message to be handled.

Parameter handler has to be of type any because Go generics only work on functions, not methods.

Returns an identifier with which the handler can be removed via UnregisterNetMessageHandler().

See also: RegisterEventHandler()
*/
func (p *parser) RegisterNetMessageHandler(handler any) dp.HandlerIdentifier {
	return p.msgDispatcher.RegisterHandler(handler)
}

// UnregisterNetMessageHandler removes a net-message handler via identifier.
//
// The identifier is returned at registration by RegisterNetMessageHandler().
func (p *parser) UnregisterNetMessageHandler(identifier dp.HandlerIdentifier) {
	p.msgDispatcher.UnregisterHandler(identifier)
}

// Close closes any open resources used by the Parser (go routines, file handles).
// This must be called before discarding the Parser to avoid memory leaks.
// Returns an error if closing of underlying resources fails.
func (p *parser) Close() error {
	p.msgDispatcher.RemoveAllQueues()

	if p.bitReader != nil {
		err := p.bitReader.Close()
		if err != nil {
			return errors.Wrap(err, "failed to close BitReader")
		}
	}

	return nil
}

func (p *parser) error() error {
	p.errLock.Lock()
	err := p.err
	p.errLock.Unlock()

	return err
}

func (p *parser) setError(err error) {
	if err == nil {
		return
	}

	p.errLock.Lock()

	if p.err != nil {
		p.errLock.Unlock()

		return
	}

	p.err = err

	p.errLock.Unlock()
}

func (p *parser) poolBitReader(r *bit.BitReader) {
	err := r.Pool()
	if err != nil {
		p.eventDispatcher.Dispatch(events.ParserWarn{
			Message: err.Error(),
		})
	}
}

// NewParser creates a new Parser with the default configuration.
// The demostream io.Reader (e.g. os.File or bytes.Reader) must provide demo data in the '.DEM' format.
//
// See also: NewCustomParser() & DefaultParserConfig
func NewParser(demostream io.Reader) Parser {
	return NewParserWithConfig(demostream, DefaultParserConfig)
}

type DemoFormat byte

const (
	DemoFormatFile DemoFormat = iota
	DemoFormatCSTVBroadcast
)

// NewCSTVBroadcastParser creates a new Parser for a live CSTV broadcast.
// The baseUrl is the base URL of the CSTV broadcast, e.g. "http://localhost:8080/s85568392932860274t1733091777".
//
// See also: NewParserWithConfig() & DefaultParserConfig
func NewCSTVBroadcastParser(baseUrl string) (Parser, error) {
	return NewCSTVBroadcastParserWithConfig(baseUrl, DefaultParserConfig)
}

// NewCSTVBroadcastParserWithConfig creates a new Parser for a live CSTV broadcast with a custom configuration.
// The baseUrl is the base URL of the CSTV broadcast, e.g. "http://localhost:8080/s85568392932860274t1733091777".
//
// See also: NewParserWithConfig() & DefaultParserConfig
func NewCSTVBroadcastParserWithConfig(baseUrl string, config ParserConfig) (Parser, error) {
	r, err := cstv.NewReader(baseUrl, config.CSTVTimeout)
	if err != nil {
		return nil, fmt.Errorf("failed to create CSTV reader: %w", err)
	}

	config.Format = DemoFormatCSTVBroadcast

	return NewParserWithConfig(r, config), nil
}

type ParserCallback func(Parser) error

// ParseWithConfig parses a demo from the given io.Reader with a custom configuration.
// The handler is called with the Parser instance.
//
// Returns an error if the parser encounters an error.
func ParseWithConfig(r io.Reader, config ParserConfig, configure ParserCallback) error {
	p := NewParserWithConfig(r, config)
	defer p.Close()

	err := configure(p)
	if err != nil {
		return fmt.Errorf("failed to configure parser: %w", err)
	}

	err = p.ParseToEnd()
	if err != nil {
		return fmt.Errorf("failed to parse demo: %w", err)
	}

	return nil
}

// Parse parses a demo from the given io.Reader.
// The handler is called with the Parser instance.
//
// Returns an error if the parser encounters an error.
func Parse(r io.Reader, configure ParserCallback) error {
	return ParseWithConfig(r, DefaultParserConfig, configure)
}

// ParseFileWithConfig parses a demo file at the given path with a custom configuration.
// The handler is called with the Parser instance.
//
// Returns an error if the file can't be opened or if the parser encounters an error.
func ParseFileWithConfig(path string, config ParserConfig, configure ParserCallback) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}

	defer f.Close()

	return ParseWithConfig(f, config, configure)
}

// ParseFile parses a demo file at the given path.
// The handler is called with the Parser instance.
//
// Returns an error if the file can't be opened or if the parser encounters an error.
func ParseFile(path string, configure ParserCallback) error {
	return ParseFileWithConfig(path, DefaultParserConfig, configure)
}

// ParseCSTVBroadcastWithConfig parses a live CSTV broadcast from the given base URL with a custom configuration.
// The handler is called with the Parser instance.
// The baseUrl is the base URL of the CSTV broadcast, e.g. "http://localhost:8080/s85568392932860274t1733091777".
// Returns an error if the CSTV reader can't be created or if the parser encounters an error.
// Note that the CSTV broadcast is a live stream and will not end until the broadcast ends.
func ParseCSTVBroadcastWithConfig(baseUrl string, config ParserConfig, configure ParserCallback) error {
	p, err := NewCSTVBroadcastParserWithConfig(baseUrl, config)
	if err != nil {
		return fmt.Errorf("failed to create CSTV broadcast parser: %w", err)
	}

	defer p.Close()

	err = configure(p)
	if err != nil {
		return fmt.Errorf("failed to configure parser: %w", err)
	}

	err = p.ParseToEnd()
	if err != nil {
		return fmt.Errorf("failed to parse CSTV broadcast: %w", err)
	}

	return nil
}

// ParseCSTVBroadcast parses a live CSTV broadcast from the given base URL.
// The handler is called with the Parser instance.
// The baseUrl is the base URL of the CSTV broadcast, e.g. "http://localhost:8080/s85568392932860274t1733091777".
// Returns an error if the CSTV reader can't be created or if the parser encounters an error.
// Note that the CSTV broadcast is a live stream and will not end until the broadcast ends.
func ParseCSTVBroadcast(baseUrl string, configure ParserCallback) error {
	return ParseCSTVBroadcastWithConfig(baseUrl, DefaultParserConfig, configure)
}

// ParserConfig contains the configuration for creating a new Parser.
type ParserConfig struct {
	// MsgQueueBufferSize defines the size of the internal net-message queue.
	// For large demos, fast i/o and slow CPUs higher numbers are suggested and vice versa.
	// The buffer size can easily be in the hundred-thousands to low millions for the best performance.
	// A negative value will make the Parser automatically decide the buffer size during parseHeader()
	// based on the number of ticks in the demo (nubmer of ticks = buffer size);
	// this is the default behavior for DefaultParserConfig.
	// Zero enforces sequential parsing.
	MsgQueueBufferSize int

	// AdditionalNetMessageCreators maps net-message-IDs to creators (instantiators).
	// The creators should return a new instance of the correct protobuf-message type (from the msg package).
	// Interesting net-message-IDs can easily be discovered with the build-tag 'debugdemoinfocs'; when looking for 'UnhandledMessage'.
	// Check out parsing.go to see which net-messages are already being parsed by default.
	// This is a beta feature and may be changed or replaced without notice.
	AdditionalNetMessageCreators map[int]NetMessageCreator

	// IgnoreErrBombsiteIndexNotFound tells the parser to not return an error when a bombsite-index from a game-event is not found in the demo.
	// See https://github.com/markus-wa/demoinfocs-golang/issues/314
	IgnoreErrBombsiteIndexNotFound bool

	// DisableMimicSource1Events tells the parser to not mimic Source 1 game events for Source 2 demos.
	// Unfortunately Source 2 demos *may* not contain Source 1 game events, that's why the parser will try to mimic them.
	// It has an impact only with Source 2 demos and is false by default.
	DisableMimicSource1Events bool

	// Source2FallbackGameEventListBin is a fallback game event list protobuf message for Source 2 demos.
	// It's used when the game event list is not found in the demo file.
	// This can happen due to a CS2 bug with sv_hibernate_when_empty.
	Source2FallbackGameEventListBin []byte

	// IgnorePacketEntitiesPanic tells the parser to ignore PacketEntities parsing panics.
	// This is required as a workaround for some POV demos that seem to contain rare PacketEntities parsing issues.
	IgnorePacketEntitiesPanic bool

	// DemoFormat is the format of the demo file (e.g. ".dem" file or live CSTV broadcast).
	Format DemoFormat

	// CSTVTimeout is the timeout for CSTV broadcasts.
	// It's the maximum time to retry for a response from the CSTV server, using an exponential backoff mechanism, starting at 1s.
	// Only used when Format is DemoFormatCSTVBroadcast.
	CSTVTimeout time.Duration
}

// DefaultParserConfig is the default Parser configuration used by NewParser().
var DefaultParserConfig = ParserConfig{
	MsgQueueBufferSize: -1,
	CSTVTimeout:        10 * time.Second,
}

// NewParserWithConfig returns a new Parser with a custom configuration.
//
// See also: NewParser() & ParserConfig
func NewParserWithConfig(demostream io.Reader, config ParserConfig) Parser {
	var p parser

	// Init parser
	p.config = config
	if p.config.Format == DemoFormatFile {
		p.bitReader = bit.NewLargeBitReader(demostream)
	} else {
		p.bitReader = bit.NewSmallBitReader(demostream)
	}
	p.equipmentMapping = make(map[st.ServerClass]common.EquipmentType)
	p.rawPlayers = make(map[int]*common.PlayerInfo)
	p.triggers = make(map[int]*boundingBoxInformation)
	p.demoInfoProvider = demoInfoProvider{parser: &p}
	p.gameState = newGameState(p.demoInfoProvider)
	p.grenadeModelIndices = make(map[int]common.EquipmentType)
	p.equipmentTypePerModel = make(map[uint64]common.EquipmentType)
	p.gameEventHandler = newGameEventHandler(&p, config.IgnoreErrBombsiteIndexNotFound)
	p.bombsiteA.index = -1
	p.bombsiteB.index = -1
	p.recordingPlayerSlot = -1
	p.disableMimicSource1GameEvents = config.DisableMimicSource1Events
	p.source2FallbackGameEventListBin = config.Source2FallbackGameEventListBin
	p.ignorePacketEntitiesPanic = config.IgnorePacketEntitiesPanic

	dispatcherCfg := dp.Config{
		PanicHandler: func(v any) {
			p.setError(fmt.Errorf("%v\nstacktrace:\n%s", v, debug.Stack()))
		},
	}
	p.msgDispatcher = dp.NewDispatcherWithConfig(dispatcherCfg)
	p.eventDispatcher = dp.NewDispatcherWithConfig(dispatcherCfg)

	p.msgDispatcher.RegisterHandler(p.handleGameEventList)
	p.msgDispatcher.RegisterHandler(p.handleGameEvent)
	p.msgDispatcher.RegisterHandler(p.handleServerInfo)
	p.msgDispatcher.RegisterHandler(p.handleCreateStringTable)
	p.msgDispatcher.RegisterHandler(p.handleUpdateStringTable)
	p.msgDispatcher.RegisterHandler(p.handleSetConVar)
	p.msgDispatcher.RegisterHandler(p.handleServerRankUpdate)
	p.msgDispatcher.RegisterHandler(p.handleMessageSayText)
	p.msgDispatcher.RegisterHandler(p.handleMessageSayText2)
	p.msgDispatcher.RegisterHandler(p.handleSendTables)
	p.msgDispatcher.RegisterHandler(p.handleFileInfo)
	p.msgDispatcher.RegisterHandler(p.handleDemoFileHeader)
	p.msgDispatcher.RegisterHandler(p.handleClassInfo)
	p.msgDispatcher.RegisterHandler(p.handleStringTables)
	p.msgDispatcher.RegisterHandler(p.handleFrameParsed)
	p.msgDispatcher.RegisterHandler(p.gameState.handleIngameTickNumber)

	if config.MsgQueueBufferSize >= 0 {
		p.initMsgQueue(config.MsgQueueBufferSize)
	}

	p.additionalNetMessageCreators = config.AdditionalNetMessageCreators

	return &p
}

func (p *parser) initMsgQueue(buf int) {
	p.msgQueue = make(chan any, buf)
	p.msgDispatcher.AddQueues(p.msgQueue)
}

type demoInfoProvider struct {
	parser *parser
}

func (p demoInfoProvider) IngameTick() int {
	return p.parser.gameState.IngameTick()
}

func (p demoInfoProvider) TickRate() float64 {
	return p.parser.TickRate()
}

func (p demoInfoProvider) FindPlayerByHandle(handle uint64) *common.Player {
	return p.parser.gameState.Participants().FindByHandle64(handle)
}

func (p demoInfoProvider) FindPlayerByPawnHandle(handle uint64) *common.Player {
	return p.parser.gameState.Participants().FindByPawnHandle(handle)
}

func (p demoInfoProvider) FindEntityByHandle(handle uint64) st.Entity {
	return p.parser.gameState.EntityByHandle(handle)
}

func (p demoInfoProvider) FindWeaponByEntityID(entityID int) *common.Equipment {
	return p.parser.gameState.weapons[entityID]
}
