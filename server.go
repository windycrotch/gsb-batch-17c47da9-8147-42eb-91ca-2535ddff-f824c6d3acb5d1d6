// Package gopher is used to start and change the core settings for the Gopher Game Server. The
// type ServerSettings contains all the parameters for changing the core settings. You can either
// pass a ServerSettings when calling Server.Start() or nil if you want to use the default server
// settings.
package gopher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hewiefreeman/GopherGameServer/actions"
	"github.com/hewiefreeman/GopherGameServer/core"
	"github.com/hewiefreeman/GopherGameServer/database"
)

/////////// TO DOs:
///////////    - Make authentication for GopherDB
///////////	- Admin tools
///////////    - More useful command-line macros

// ServerSettings are the core settings for the Gopher Game Server. You must fill one of these out to customize
// the server's functionality to your liking.
type ServerSettings struct {
	ServerName     string // The server's name. Used for the server's ownership of private Rooms. (Required)
	MaxConnections int    // The maximum amount of concurrent connections the server will accept. Setting this to 0 means infinite.

	HostName  string // Server's host name. Use 'https://' for TLS connections. (ex: 'https://example.com') (Required)
	HostAlias string // Server's host alias name. Use 'https://' for TLS connections. (ex: 'https://www.example.com')
	IP        string // Server's IP address. (Required)
	Port      int    // Server's port. (Required)

	TLS         bool   // Enables TLS/SSL connections.
	CertFile    string // SSL/TLS certificate file location (starting from system's root folder). (Required for TLS)
	PrivKeyFile string // SSL/TLS private key file location (starting from system's root folder). (Required for TLS)

	OriginOnly bool // When enabled, the server declines connections made from outside the origin server (Admin logins always check origin). IMPORTANT: Enable this for web apps and LAN servers.

	MultiConnect   bool  // Enables multiple connections under the same User. When enabled, will override KickDupOnLogin's functionality.
	MaxUserConns   uint8 // Overrides the default (255) of maximum simultaneous connections on a single User
	KickDupOnLogin bool  // When enabled, a logged in User will be disconnected from service when another User logs in with the same name.

	UserRoomControl   bool // Enables Users to create Rooms, invite/uninvite(AKA revoke) other Users to their owned private rooms, and destroy their owned rooms.
	RoomDeleteOnLeave bool // When enabled, Rooms created by a User will be deleted when the owner leaves. WARNING: If disabled, you must remember to at some point delete the rooms created by Users, or they will pile up endlessly!

	EnableSqlFeatures bool   // Enables the built-in SQL User authentication and friending. NOTE: It is HIGHLY recommended to use TLS over an SSL/HTTPS connection when using the SQL features. Otherwise, sensitive User information can be compromised with network "snooping" (AKA "sniffing").
	SqlIP             string // SQL Database IP address. (Required for SQL features)
	SqlPort           int    // SQL Database port. (Required for SQL features)
	SqlProtocol       string // The protocol to use while comminicating with the MySQL database. Most use either 'udp' or 'tcp'. (Required for SQL features)
	SqlUser           string // SQL user name (Required for SQL features)
	SqlPassword       string // SQL user password (Required for SQL features)
	SqlDatabase       string // SQL database name (Required for SQL features)
	EncryptionCost    int    // The amount of encryption iterations the server will run when storing and checking passwords. The higher the number, the longer encryptions take, but are more secure. Default is 4, range is 4-31.
	CustomLoginColumn string // The custom AccountInfoColumn you wish to use for logging in instead of the default name column.
	RememberMe        bool   // Enables the "Remember Me" login feature. You can read more about this in project's wiki.

	EnableRecovery   bool   // Enables the recovery of all Rooms, their settings, and their variables on start-up after terminating the server.
	RecoveryLocation string // The folder location (starting from system's root folder) where you would like to store the recovery data. (Required for recovery)

	AdminLogin    string // The login name for the Admin Tools (Required for Admin Tools)
	AdminPassword string // The password for the Admin Tools (Required for Admin Tools)
}

type serverRestore struct {
	R map[string]core.RoomRecoveryState
}

var (
	httpServer *http.Server

	settings *ServerSettings

	serverStarted  bool       = false
	serverPaused   bool       = false
	serverStopping bool       = false
	serverEndChan  chan error = make(chan error)

	// lifeMu serializes Start/ShutDown lifecycle transitions and guards the
	// shutdown coordination fields below.
	lifeMu sync.Mutex
	// shutdownDone is closed once the single shutdown for the current
	// generation has finished. Concurrent/duplicate ShutDown calls share it.
	shutdownDone chan struct{}
	shutdownErr  error
	// startExited is closed when the Start goroutine for a generation returns.
	startExited chan struct{}

	// writeMu serializes persistence commits so concurrent saveState,
	// writeState, and ShutDown calls can never write overlapping files.
	writeMu sync.Mutex

	// lastRecoveryReport records the outcome of the most recent recoverState
	// run. It is exposed through LastRecoveryReport for observability/tests.
	lastRecoveryReport core.RecoveryReport

	startCallback         func()
	pauseCallback         func()
	stopCallback          func()
	resumeCallback        func()
	clientConnectCallback func(*http.ResponseWriter, *http.Request) bool

	//SERVER VERSION NUMBER
	version string = "1.0-BETA.2"
)

// Recovery file naming. Recovery files keep the historical
// "Gopher Recovery - <timestamp>.grf" format so archives produced by older
// builds (and test fixtures using that naming scheme) are still discovered.
const (
	recoveryFilePrefix = "Gopher Recovery"
	recoveryFileSuffix = ".grf"
	recoveryTimeLayout = "2006-01-02 15-04-05"
)

// LastRecoveryReport returns a summary of the most recent start-up recovery
// attempt: which archive was selected, how many rooms were restored, which
// rooms failed, and which corrupt archives were skipped.
func LastRecoveryReport() core.RecoveryReport {
	return lastRecoveryReport
}

//////////////////////////////////////////////////////////////////////////////////////////////////////
//   Server start-up   ///////////////////////////////////////////////////////////////////////////////
//////////////////////////////////////////////////////////////////////////////////////////////////////

// Start will start the server. Call with a pointer to your `ServerSettings` (or nil for defaults) to start the server. The default
// settings are for local testing ONLY. There are security-related options in `ServerSettings`
// for SSL/TLS, connection origin testing, administrator tools, and more. It's highly recommended to look into
// all `ServerSettings` options to tune the server for your desired functionality and security needs.
//
// This function will block the thread that it is ran on until the server either errors, or is manually shut-down. To run code after the
// server starts/stops/pauses/etc, use the provided server callback setter functions.
func Start(s *ServerSettings) {
	lifeMu.Lock()
	if serverStarted || serverPaused {
		lifeMu.Unlock()
		return
	}
	serverStarted = true
	serverStopping = false
	shutdownDone = nil
	shutdownErr = nil
	serverEndChan = make(chan error, 1)
	startExited = make(chan struct{})
	lifeMu.Unlock()

	// Signal complete exit of this Start generation so ShutDown can wait for
	// its tail work before returning, preventing cross-generation state races.
	defer close(startExited)
	fmt.Println("  _______                __\n |   _   |.-----..-----.|  |--..-----..----.\n |.  |___||. _  ||. _  ||.    ||. -__||.  _|\n |.  |   ||:. . ||:. __||: |: ||:    ||: |\n |:  |   |'-----'|: |   '--'--''-----''--'\n |::.. . |       '--' - Game Server -\n '-------'\n\n ")
	fmt.Println("Starting server...")
	// Set server settings
	if s != nil {
		if !s.verify() {
			lifeMu.Lock()
			serverStarted = false
			lifeMu.Unlock()
			return
		}
		settings = s
	} else {
		// Default localhost settings
		fmt.Println("Using default settings...")
		settings = &ServerSettings{
			ServerName:     "!server!",
			MaxConnections: 0,

			HostName:  "localhost",
			HostAlias: "localhost",
			IP:        "localhost",
			Port:      8080,

			TLS:         false,
			CertFile:    "",
			PrivKeyFile: "",

			OriginOnly: false,

			MultiConnect:   false,
			KickDupOnLogin: false,

			UserRoomControl:   true,
			RoomDeleteOnLeave: true,

			EnableSqlFeatures: false,
			SqlIP:             "localhost",
			SqlPort:           3306,
			SqlProtocol:       "tcp",
			SqlUser:           "user",
			SqlPassword:       "password",
			SqlDatabase:       "database",
			EncryptionCost:    4,
			CustomLoginColumn: "",
			RememberMe:        false,

			EnableRecovery:   false,
			RecoveryLocation: "C:/",

			AdminLogin:    "admin",
			AdminPassword: "password"}
	}

	// Update package settings
	core.SettingsSet((*settings).KickDupOnLogin, (*settings).ServerName, (*settings).RoomDeleteOnLeave, (*settings).EnableSqlFeatures,
		(*settings).RememberMe, (*settings).MultiConnect, (*settings).MaxUserConns)

	// Clear any paused/stopped state left over from a previous in-process
	// generation before marking the packages as started.
	core.Resume()
	actions.Resume()
	database.Resume()

	// Reset connection accounting for the new generation.
	conns.reset()

	// Notify packages of server start
	core.SetServerStarted(true)
	actions.SetServerStarted(true)
	database.SetServerStarted(true)

	// Start database
	if (*settings).EnableSqlFeatures {
		fmt.Println("Initializing database...")
		dbErr := database.Init((*settings).SqlUser, (*settings).SqlPassword, (*settings).SqlDatabase,
			(*settings).SqlProtocol, (*settings).SqlIP, (*settings).SqlPort, (*settings).EncryptionCost,
			(*settings).RememberMe, (*settings).CustomLoginColumn)
		if dbErr != nil {
			fmt.Println("Database error:", dbErr.Error())
			fmt.Println("Shutting down...")
			lifeMu.Lock()
			serverStarted = false
			lifeMu.Unlock()
			core.SetServerStarted(false)
			actions.SetServerStarted(false)
			database.SetServerStarted(false)
			return
		}
		fmt.Println("Database initialized")
	}

	// Recover state
	if settings.EnableRecovery {
		recoverState()
	}

	// Start socket listener
	var srv *http.Server
	if settings.TLS {
		srv = makeServer("/wss", settings.TLS)
	} else {
		srv = makeServer("/ws", settings.TLS)
	}
	lifeMu.Lock()
	httpServer = srv
	lifeMu.Unlock()

	// Run callback
	if startCallback != nil {
		startCallback()
	}

	// Start macro listener
	go macroListener()

	fmt.Println("Startup complete")

	// Wait for server shutdown
	doneErr := <-serverEndChan

	if doneErr != nil && doneErr != http.ErrServerClosed {
		fmt.Println("Fatal server error:", doneErr.Error())

		lifeMu.Lock()
		alreadyStopping := serverStopping
		if !serverStopping {
			serverStopping = true
			if shutdownDone == nil {
				shutdownDone = make(chan struct{})
			}
		}
		done := shutdownDone
		lifeMu.Unlock()

		if !alreadyStopping {
			fmt.Println("Disconnecting users...")

			// Pause server
			core.Pause()
			actions.Pause()
			database.Pause()

			// Reject new connections and drop current ones.
			conns.sealAndCloseAll()
			conns.waitZero(2 * time.Second)
			conns.forceZero()

			// Save state
			if settings != nil && settings.EnableRecovery {
				if saveErr := saveState(); saveErr != nil {
					lifeMu.Lock()
					shutdownErr = saveErr
					lifeMu.Unlock()
				}
			}

			lifeMu.Lock()
			serverStarted = false
			serverPaused = false
			if shutdownDone != nil {
				close(shutdownDone)
			}
			lifeMu.Unlock()
			_ = done
		}
	} else {
		// Graceful ShutDown already cleared the started flag; make sure a
		// blocked lifecycle observer sees a consistent stopped state.
		lifeMu.Lock()
		serverStarted = false
		serverPaused = false
		lifeMu.Unlock()
	}

	fmt.Println("Server shut-down completed")

	if stopCallback != nil {
		stopCallback()
	}
}

func (settings *ServerSettings) verify() bool {
	if settings.ServerName == "" {
		fmt.Println("ServerName in ServerSettings is required. Shutting down...")
		return false

	} else if settings.HostName == "" || settings.IP == "" || settings.Port < 1 {
		fmt.Println("HostName, IP, and Port in ServerSettings are required. Shutting down...")
		return false

	} else if settings.TLS == true && (settings.CertFile == "" || settings.PrivKeyFile == "") {
		fmt.Println("CertFile and PrivKeyFile in ServerSettings are required for a TLS connection. Shutting down...")
		return false

	} else if settings.EnableSqlFeatures == true && (settings.SqlIP == "" || settings.SqlPort < 1 || settings.SqlProtocol == "" ||
		settings.SqlUser == "" || settings.SqlPassword == "" || settings.SqlDatabase == "") {
		fmt.Println("SqlIP, SqlPort, SqlProtocol, SqlUser, SqlPassword, and SqlDatabase in ServerSettings are required for the SQL features. Shutting down...")
		return false

	} else if settings.EnableRecovery == true && settings.RecoveryLocation == "" {
		fmt.Println("RecoveryLocation in ServerSettings is required for server recovery. Shutting down...")
		return false

	} else if settings.EnableRecovery {
		// Check if invalid file location
		if _, err := os.Stat(settings.RecoveryLocation); err != nil {
			fmt.Println("RecoveryLocation error:", err)
			fmt.Println("Shutting down...")
			return false
		}
		var d []byte
		if err := ioutil.WriteFile(settings.RecoveryLocation+"/test.txt", d, 0644); err != nil {
			fmt.Println("RecoveryLocation error:", err)
			fmt.Println("Shutting down...")
			return false
		}
		os.Remove(settings.RecoveryLocation + "/test.txt")

	} else if settings.AdminLogin == "" || settings.AdminPassword == "" {
		fmt.Println("AdminLogin and AdminPassword in ServerSettings are required. Shutting down...")
		return false
	}

	return true
}

func makeServer(handleDir string, tls bool) *http.Server {
	// Use a dedicated ServeMux instead of http.DefaultServeMux so starting a
	// second generation of the server in the same process does not panic with
	// a duplicate route registration.
	mux := http.NewServeMux()
	mux.HandleFunc(handleDir, socketInitializer)
	server := &http.Server{Addr: settings.IP + ":" + strconv.Itoa(settings.Port), Handler: mux}
	endChan := serverEndChan
	if tls {
		go func() {
			err := server.ListenAndServeTLS(settings.CertFile, settings.PrivKeyFile)
			endChan <- err
		}()
	} else {
		go func() {
			err := server.ListenAndServe()
			endChan <- err
		}()
	}

	//
	return server
}

//////////////////////////////////////////////////////////////////////////////////////////////////////
//   Server actions   ////////////////////////////////////////////////////////////////////////////////
//////////////////////////////////////////////////////////////////////////////////////////////////////

// Pause will log all Users off and prevent anyone from logging in. All rooms and their variables created by the server will remain in memory.
// Same goes for rooms created by Users unless RoomDeleteOnLeave in ServerSettings is set to true.
func Pause() {
	lifeMu.Lock()
	if serverPaused {
		lifeMu.Unlock()
		return
	}
	serverPaused = true
	serverStarted = false
	lifeMu.Unlock()

	fmt.Println("Pausing server...")

	core.Pause()
	actions.Pause()
	database.Pause()

	// Run callback
	if pauseCallback != nil {
		pauseCallback()
	}

	fmt.Println("Server paused")
}

// Resume will allow Users to login again after pausing the server.
func Resume() {
	lifeMu.Lock()
	if !serverPaused {
		lifeMu.Unlock()
		return
	}
	serverPaused = false
	serverStarted = true
	lifeMu.Unlock()

	fmt.Println("Resuming server...")
	core.Resume()
	actions.Resume()
	database.Resume()

	// Run callback
	if resumeCallback != nil {
		resumeCallback()
	}

	fmt.Println("Server resumed")
}

// ShutDown will log all Users off, save the state of the server if EnableRecovery in ServerSettings is set to true, then shut the server down.
//
// ShutDown is safe to call concurrently and repeatedly: all callers join the
// same shutdown sequence and receive the same result. The state is persisted
// exactly once per server generation, atomically, after mutation entry points
// have been paused. When it returns, no new client connections are accepted
// and ClientsConnected is guaranteed to be zero.
func ShutDown() error {
	lifeMu.Lock()
	if serverStopping {
		done := shutdownDone
		lifeMu.Unlock()
		if done != nil {
			<-done
		}
		lifeMu.Lock()
		err := shutdownErr
		lifeMu.Unlock()
		return err
	}
	serverStopping = true
	shutdownErr = nil
	shutdownDone = make(chan struct{})
	done := shutdownDone
	srv := httpServer
	cfg := settings
	exited := startExited
	lifeMu.Unlock()

	var returnErr error

	fmt.Println("Disconnecting users...")

	// Pause core entry points first so no new logins/actions mutate room
	// state while the snapshot is taken.
	core.Pause()
	actions.Pause()
	database.Pause()

	// Reject new connections and close existing sockets so their goroutines
	// wind down and release connection counts.
	conns.sealAndCloseAll()

	// Save state once, serialized against any concurrent saveState call.
	if cfg != nil && cfg.EnableRecovery {
		if saveErr := saveState(); saveErr != nil {
			fmt.Println("Error saving state:", saveErr)
			returnErr = saveErr
		}
	}

	// Shut the HTTP listener down. A nil server means Start never reached the
	// listener (or ShutDown was called standalone); there is nothing to close.
	if srv != nil {
		fmt.Println("Shutting server down...")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		shutdownErrHTTP := srv.Shutdown(ctx)
		cancel()
		if shutdownErrHTTP != nil && shutdownErrHTTP != http.ErrServerClosed && returnErr == nil {
			returnErr = shutdownErrHTTP
		}
	} else {
		fmt.Println("Shutting server down...")
	}

	// Wait for the Start goroutine to finish its shutdown tail so no stale
	// generation writes lifecycle state after we return.
	if exited != nil {
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
	}

	// Wait for hijacked websocket goroutines to release their counts, then
	// reconcile to zero so callers observe a deterministic empty counter.
	conns.waitZero(2 * time.Second)
	conns.forceZero()

	// Return the core packages to their pre-start configuration state so a
	// subsequent Start in the same process behaves like a fresh generation.
	core.SetServerStarted(false)
	actions.SetServerStarted(false)
	database.SetServerStarted(false)

	lifeMu.Lock()
	serverStarted = false
	serverPaused = false
	shutdownErr = returnErr
	close(done)
	lifeMu.Unlock()

	return returnErr
}

//////////////////////////////////////////////////////////////////////////////////////////////////////
//   Saving and recovery   ///////////////////////////////////////////////////////////////////////////
//////////////////////////////////////////////////////////////////////////////////////////////////////

// saveState persists the current in-memory room state. It is safe to call
// concurrently with ShutDown or other saveState calls: commits are serialized
// and every file is written atomically, so observers never see a truncated or
// interleaved recovery file.
func saveState() error {
	fmt.Println("Saving server state...")
	folder := ""
	if settings != nil {
		folder = settings.RecoveryLocation
	}

	saveErr := writeState(getState(), folder)
	if saveErr != nil {
		fmt.Println("Error saving state:", saveErr)
		return saveErr
	}
	fmt.Println("Save state successful")
	return nil
}

func writeState(stateObj serverRestore, saveFolder string) error {
	state, err := json.Marshal(stateObj)
	if err != nil {
		return err
	}
	fileName := recoveryFilePrefix + " - " + time.Now().Format(recoveryTimeLayout) + recoveryFileSuffix
	finalPath := filepath.Join(saveFolder, fileName)

	// Serialize commits; the temp-file + atomic rename below additionally
	// guarantees no recognized path is ever observed half-written.
	writeMu.Lock()
	defer writeMu.Unlock()

	if err := atomicWriteFile(finalPath, state); err != nil {
		return err
	}

	return nil
}

// atomicWriteFile writes data to a unique temporary file in the same directory
// and atomically renames it onto finalPath. A crash or concurrent writer can
// therefore never leave a half-written file at a recognized recovery path.
func atomicWriteFile(finalPath string, data []byte) (err error) {
	dir := filepath.Dir(finalPath)
	tmp, err := ioutil.TempFile(dir, ".grf-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Best-effort durability flush before the atomic rename.
	_ = tmp.Sync()
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpName, finalPath); err != nil {
		return err
	}
	committed = true
	return nil
}

func getState() serverRestore {
	return serverRestore{
		R: core.GetRoomsState(),
	}
}

// recoveryCandidates returns all recognized recovery archives in folder,
// newest modification time first. Equal modification times are ordered by
// descending file name for deterministic behavior.
func recoveryCandidates(folder string) ([]string, error) {
	files, err := ioutil.ReadDir(folder)
	if err != nil {
		return nil, err
	}

	type candidate struct {
		path    string
		name    string
		modTime int64
	}
	candidates := make([]candidate, 0, len(files))
	for _, f := range files {
		name := f.Name()
		if f.IsDir() || !strings.HasPrefix(name, recoveryFilePrefix) {
			continue
		}
		fullPath := filepath.Join(folder, name)
		modTime := f.ModTime().UnixNano()
		// Prefer a stat result in case ReadDir info is stale.
		if fi, statErr := os.Stat(fullPath); statErr == nil {
			modTime = fi.ModTime().UnixNano()
		}
		candidates = append(candidates, candidate{path: fullPath, name: name, modTime: modTime})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].modTime != candidates[j].modTime {
			return candidates[i].modTime > candidates[j].modTime
		}
		return candidates[i].name > candidates[j].name
	})

	paths := make([]string, len(candidates))
	for i, c := range candidates {
		paths[i] = c.path
	}
	return paths, nil
}

// readRecoveryArchive reads and parses one archive. A non-nil error means the
// file is unreadable or is not a complete, parseable recovery document.
func readRecoveryArchive(path string) (serverRestore, error) {
	var recovery serverRestore
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return recovery, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return recovery, errors.New("recovery file is empty")
	}
	if err := json.Unmarshal(data, &recovery); err != nil {
		return recovery, err
	}
	return recovery, nil
}

func recoverState() {
	fmt.Println("Recovering previous state...")

	report := core.RecoveryReport{Failed: make(map[string]error)}
	lastRecoveryReport = report

	folder := ""
	if settings != nil {
		folder = settings.RecoveryLocation
	}

	candidates, err := recoveryCandidates(folder)
	if err != nil {
		fmt.Println("Error reading recovery location:", err)
		lastRecoveryReport = report
		return
	}

	// Try archives from newest to oldest, falling back to earlier valid ones
	// whenever the newest is missing, truncated, or corrupt.
	var recovery serverRestore
	var selectedPath string
	for _, path := range candidates {
		candidate, readErr := readRecoveryArchive(path)
		if readErr != nil {
			name := filepath.Base(path)
			report.SkippedArchives = append(report.SkippedArchives, name)
			fmt.Println("Skipping invalid recovery file '"+name+"':", readErr)
			continue
		}
		recovery = candidate
		selectedPath = path
		break
	}

	if selectedPath == "" {
		fmt.Println("No valid recovery archive found; no rooms to restore.")
		lastRecoveryReport = report
		return
	}

	report.Archive = filepath.Base(selectedPath)
	fmt.Println("Using recovery archive:", report.Archive)

	// An archive is the source of truth for a start-up generation. Reset the
	// in-memory registry first so restarting in one process (where the old
	// rooms are still mapped) does not collide with the rooms being restored.
	core.ResetRooms()

	if recovery.R == nil || len(recovery.R) == 0 {
		fmt.Println("No rooms to restore!")
		lastRecoveryReport = report
		return
	}

	// Recover rooms independently. A failure for one room rolls back any
	// partial state for that room but does not abort the remaining rooms.
	for name, val := range recovery.R {
		if roomErr := core.RestoreRoom(name, val); roomErr != nil {
			report.Failed[name] = roomErr
			fmt.Println("Error recovering room '"+name+"':", roomErr)
			continue
		}
		report.Restored++
	}

	lastRecoveryReport = report
	fmt.Println(report.String())
	if len(report.Failed) == 0 {
		fmt.Println("State recovery successful")
	} else {
		fmt.Printf("State recovery finished with %d restored and %d failed room(s)\n", report.Restored, len(report.Failed))
	}
}
