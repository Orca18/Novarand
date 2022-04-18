// Copyright (C) 2019-2022 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/algorand/go-deadlock"
	"github.com/gofrs/flock"

	"github.com/Orca18/novarand/config"
	"github.com/Orca18/novarand/crypto"
	"github.com/Orca18/novarand/daemon/algod"
	"github.com/Orca18/novarand/data/bookkeeping"
	"github.com/Orca18/novarand/logging"
	"github.com/Orca18/novarand/logging/telemetryspec"
	"github.com/Orca18/novarand/network"
	"github.com/Orca18/novarand/protocol"
	toolsnet "github.com/Orca18/novarand/tools/network"
	"github.com/Orca18/novarand/util/metrics"
	"github.com/Orca18/novarand/util/tokens"
)

var dataDirectory = flag.String("d", "", "Root Algorand daemon data path")
var genesisFile = flag.String("g", "", "Genesis configuration file")
var genesisPrint = flag.Bool("G", false, "Print genesis ID")
var versionCheck = flag.Bool("v", false, "Display and write current build version and exit")
var branchCheck = flag.Bool("b", false, "Display the git branch behind the build")
var channelCheck = flag.Bool("c", false, "Display and release channel behind the build")
var initAndExit = flag.Bool("x", false, "Initialize the ledger and exit")
var peerOverride = flag.String("p", "", "Override phonebook with peer ip:port (or semicolon separated list: ip:port;ip:port;ip:port...)")
var listenIP = flag.String("l", "", "Override config.EndpointAddress (REST listening address) with ip:port")
var sessionGUID = flag.String("s", "", "Telemetry Session GUID to use")
var telemetryOverride = flag.String("t", "", `Override telemetry setting if supported (Use "true", "false", "0" or "1"`)
var seed = flag.String("seed", "", "input to math/rand.Seed()")

func main() {
	flag.Parse()
	exitCode := run()
	os.Exit(exitCode)
}

func run() int {
	dataDir := resolveDataDir()
	fmt.Println("데이터디렉토리", dataDir)
	dataDir = "/root/hknode/net1"
	fmt.Println("데이터디렉토리", dataDir)
	absolutePath, absPathErr := filepath.Abs(dataDir)
	config.UpdateVersionDataDir(absolutePath)

	if *seed != "" {
		println("seed 포인터 비었는가?안비었다.")
		seedVal, err := strconv.ParseInt(*seed, 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad seed %#v: %s\n", *seed, err)
			return 1
		}
		rand.Seed(seedVal)
		println("안빈 경우 포인터에서 가져와 할당", seedVal)
	} else {
		println("seed 포인터 비었는가?비었다.")
		rand.Seed(time.Now().UnixNano())
		println("빈 경우 시간으로 생성", time.Now().UnixNano())
	}

	if *versionCheck {
		fmt.Println(config.FormatVersionAndLicense())
		return 0
	}

	version := config.GetCurrentVersion()
	heartbeatGauge := metrics.MakeStringGauge()
	heartbeatGauge.Set("version", version.String())
	heartbeatGauge.Set("version-num", strconv.FormatUint(version.AsUInt64(), 10))
	heartbeatGauge.Set("channel", version.Channel)
	heartbeatGauge.Set("branch", version.Branch)
	heartbeatGauge.Set("commit-hash", version.GetCommitHash())

	if *branchCheck {
		fmt.Println(config.Branch)
		return 0
	}

	if *channelCheck {
		fmt.Println(config.Channel)
		return 0
	}

	// Don't fallback anymore - if not specified, we want to panic to force us to update our tooling and/or processes
	// 더 이상 폴백하지 마십시오. 지정하지 않으면 도구 및/또는 프로세스를 업데이트하도록 패닉 상태에 빠지고 싶습니다.
	if len(dataDir) == 0 {
		fmt.Fprintln(os.Stderr, "Data directory not specified.  Please use -d or set $ALGORAND_DATA in your environment.")
		return 1
	}

	if absPathErr != nil {
		fmt.Fprintf(os.Stderr, "Can't convert data directory's path to absolute, %v\n", dataDir)
		return 1
	}

	genesisPath := *genesisFile
	if genesisPath == "" {
		genesisPath = filepath.Join(dataDir, config.GenesisJSONFile)
	}

	// Load genesis
	genesisText, err := ioutil.ReadFile(genesisPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot read genesis file %s: %v\n", genesisPath, err)
		return 1
	}

	var genesis bookkeeping.Genesis
	err = protocol.DecodeJSON(genesisText, &genesis)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot parse genesis file %s: %v\n", genesisPath, err)
		return 1
	}

	if *genesisPrint {
		fmt.Println(genesis.ID())
		return 0
	}

	// If data directory doesn't exist, we can't run. Don't bother trying.
	// 데이터 디렉토리가 없으면 실행할 수 없습니다. 귀찮게 시도하지 마십시오.
	if _, err := os.Stat(absolutePath); err != nil {
		fmt.Fprintf(os.Stderr, "Data directory %s does not appear to be valid\n", dataDir)
		return 1
	}

	log := logging.Base()
	// before doing anything further, attempt to acquire the algod lock to ensure this is the only node running against this data directory
	// 더 이상의 작업을 수행하기 전에 이 데이터 디렉토리에 대해 실행 중인 유일한 노드인지 확인하기 위해 algod 잠금을 획득하려고 시도합니다.
	lockPath := filepath.Join(absolutePath, "algod.lock")
	fileLock := flock.New(lockPath)
	locked, err := fileLock.TryLock()
	if err != nil {
		fmt.Fprintf(os.Stderr, "unexpected failure in establishing algod.lock: %s \n", err.Error())
		return 1
	}
	if !locked {
		fmt.Fprintln(os.Stderr, "failed to lock algod.lock; is an instance of algod already running in this data directory?")
		return 1
	}
	defer fileLock.Unlock()

	cfg, err := config.LoadConfigFromDisk(absolutePath)
	if err != nil && !os.IsNotExist(err) {
		// log is not setup yet, this will log to stderr
		// 로그가 아직 설정되지 않았으므로 stderr에 기록됩니다.
		log.Fatalf("Cannot load config: %v", err)
	}

	err = config.LoadConfigurableConsensusProtocols(absolutePath)
	if err != nil {
		// log is not setup yet, this will log to stderr
		// 로그가 아직 설정되지 않았으므로 stderr에 기록됩니다.
		log.Fatalf("Unable to load optional consensus protocols file: %v", err)
	}

	// Enable telemetry hook in daemon to send logs to cloud
	// If ALGOTEST env variable is set, telemetry is disabled - allows disabling telemetry for tests
	// 로그를 클라우드로 보내기 위해 데몬에서 원격 측정 후크를 활성화합니다.
	// ALGOTEST 환경 변수가 설정되면 원격 측정이 비활성화됩니다. 테스트를 위해 원격 측정을 비활성화할 수 있습니다.
	isTest := os.Getenv("ALGOTEST") != ""
	remoteTelemetryEnabled := false
	if !isTest {
		telemetryConfig, err := logging.EnsureTelemetryConfig(&dataDir, genesis.ID())
		if err != nil {
			fmt.Fprintln(os.Stdout, "error loading telemetry config", err)
		}
		if os.IsPermission(err) {
			fmt.Fprintf(os.Stderr, "Permission error on accessing telemetry config: %v", err)
			return 1
		}
		fmt.Fprintf(os.Stdout, "Telemetry configured from '%s'\n", telemetryConfig.FilePath)

		telemetryConfig.SendToLog = telemetryConfig.SendToLog || cfg.TelemetryToLog

		// Apply telemetry override.
		// 원격 측정 재정의를 적용합니다.
		telemetryConfig.Enable = logging.TelemetryOverride(*telemetryOverride, &telemetryConfig)
		remoteTelemetryEnabled = telemetryConfig.Enable

		if telemetryConfig.Enable || telemetryConfig.SendToLog {
			// If session GUID specified, use it.
			// 세션 GUID가 지정된 경우 사용합니다.
			if *sessionGUID != "" {
				if len(*sessionGUID) == 36 {
					telemetryConfig.SessionGUID = *sessionGUID
				}
			}
			err = log.EnableTelemetry(telemetryConfig)
			if err != nil {
				fmt.Fprintln(os.Stdout, "error creating telemetry hook", err)
			}
		}
	}

	s := algod.Server{
		RootPath: absolutePath,
		Genesis:  genesis,
	}

	// Generate a REST API token if one was not provided
	// REST API 토큰이 제공되지 않은 경우 생성
	apiToken, wroteNewToken, err := tokens.ValidateOrGenerateAPIToken(s.RootPath, tokens.AlgodTokenFilename)

	if err != nil {
		log.Fatalf("API token error: %v", err)
	}

	if wroteNewToken {
		fmt.Printf("No REST API Token found. Generated token: %s\n", apiToken)
	}

	// Generate a admin REST API token if one was not provided
	// 관리자 REST API 토큰이 제공되지 않은 경우 생성
	adminAPIToken, wroteNewToken, err := tokens.ValidateOrGenerateAPIToken(s.RootPath, tokens.AlgodAdminTokenFilename)

	if err != nil {
		log.Fatalf("Admin API token error: %v", err)
	}

	if wroteNewToken {
		fmt.Printf("No Admin REST API Token found. Generated token: %s\n", adminAPIToken)
	}

	// Allow overriding default listening address
	// 기본 수신 주소 재정의 허용
	if *listenIP != "" {
		cfg.EndpointAddress = *listenIP
	}

	// If overriding peers, disable SRV lookup
	// 피어를 재정의하는 경우 SRV 조회를 비활성화합니다.
	telemetryDNSBootstrapID := cfg.DNSBootstrapID
	var peerOverrideArray []string
	if *peerOverride != "" {
		peerOverrideArray = strings.Split(*peerOverride, ";")
		cfg.DNSBootstrapID = ""

		// The networking code waits until we have GossipFanout connections before declaring the network stack to be ready, which triggers things like catchup.
		// If the user explicitly specified a set of peers, make sure
		// GossipFanout is no larger than this set, otherwise we will have to wait for a minute-long timeout until the network stack declares itself to be ready.
		// 네트워킹 코드는 네트워크 스택이 준비되었다고 선언하기 전에 GossipFanout 연결이 있을 때까지 기다립니다. 이는 캐치업과 같은 것을 트리거합니다.
		// 사용자가 피어 집합을 명시적으로 지정한 경우
		// GossipFanout은 이 세트보다 크지 않습니다. 그렇지 않으면 네트워크 스택이 스스로 준비가 되었다고 선언할 때까지 1분의 시간 초과를 기다려야 합니다.
		if cfg.GossipFanout > len(peerOverrideArray) {
			cfg.GossipFanout = len(peerOverrideArray)
		}

		// make sure that the format of each entry is valid:
		// 각 항목의 형식이 유효한지 확인합니다.
		for idx, peer := range peerOverrideArray {
			url, err := network.ParseHostOrURL(peer)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Provided command line parameter '%s' is not a valid host:port pair\n", peer)
				return 1
			}
			peerOverrideArray[idx] = url.Host
		}
	}

	// Apply the default deadlock setting before starting the server.
	// It will potentially override it based on the config file DefaultDeadlock setting
	// 서버를 시작하기 전에 기본 교착 상태 설정을 적용합니다.
	// 구성 파일 DefaultDeadlock 설정을 기반으로 잠재적으로 재정의합니다.
	if strings.ToLower(config.DefaultDeadlock) == "enable" {
		deadlock.Opts.Disable = false
	} else if strings.ToLower(config.DefaultDeadlock) == "disable" {
		deadlock.Opts.Disable = true
	} else if config.DefaultDeadlock != "" {
		log.Fatalf("DefaultDeadlock is somehow not set to an expected value (enable / disable): %s", config.DefaultDeadlock)
	}

	var phonebookAddresses []string
	if peerOverrideArray != nil {
		phonebookAddresses = peerOverrideArray
	} else {
		ex, err := os.Executable()
		if err != nil {
			log.Errorf("cannot locate node executable: %s", err)
		} else {
			phonebookDir := filepath.Dir(ex)
			phonebookAddresses, err = config.LoadPhonebook(phonebookDir)
			if err != nil {
				log.Debugf("Cannot load static phonebook: %v", err)
			}
		}
	}

	err = s.Initialize(cfg, phonebookAddresses, string(genesisText))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		log.Error(err)
		return 1
	}

	if *initAndExit {
		return 0
	}

	deadlockState := "enabled"
	if deadlock.Opts.Disable {
		deadlockState = "disabled"
	}
	fmt.Fprintf(os.Stdout, "Deadlock detection is set to: %s (Default state is '%s')\n", deadlockState, config.DefaultDeadlock)

	if log.GetTelemetryEnabled() {
		done := make(chan struct{})
		defer close(done)

		// Make a copy of config and reset DNSBootstrapID in case it was disabled.
		// 구성의 복사본을 만들고 비활성화된 경우 DNSBootstrapID를 재설정합니다.
		cfgCopy := cfg
		cfgCopy.DNSBootstrapID = telemetryDNSBootstrapID

		// If the telemetry URI is not set, periodically check SRV records for new telemetry URI
		// 원격 측정 URI가 설정되지 않은 경우 SRV 레코드에서 새로운 원격 측정 URI를 주기적으로 확인합니다.
		if remoteTelemetryEnabled && log.GetTelemetryURI() == "" {
			toolsnet.StartTelemetryURIUpdateService(time.Minute, cfg, s.Genesis.Network, log, done)
		}

		currentVersion := config.GetCurrentVersion()
		startupDetails := telemetryspec.StartupEventDetails{
			Version:      currentVersion.String(),
			CommitHash:   currentVersion.CommitHash,
			Branch:       currentVersion.Branch,
			Channel:      currentVersion.Channel,
			InstanceHash: crypto.Hash([]byte(absolutePath)).String(),
		}

		log.EventWithDetails(telemetryspec.ApplicationState, telemetryspec.StartupEvent, startupDetails)

		// Send a heartbeat event every 10 minutes as a sign of life
		// 생명의 표시로 10분마다 하트비트 이벤트를 보냅니다.
		go func() {
			ticker := time.NewTicker(10 * time.Minute)
			defer ticker.Stop()

			sendHeartbeat := func() {
				values := make(map[string]string)
				metrics.DefaultRegistry().AddMetrics(values)

				heartbeatDetails := telemetryspec.HeartbeatEventDetails{
					Metrics: values,
				}

				log.EventWithDetails(telemetryspec.ApplicationState, telemetryspec.HeartbeatEvent, heartbeatDetails)
			}

			// Send initial heartbeat, followed by one every 10 minutes.
			// 초기 하트비트를 보낸 후 10분마다 하나씩 보냅니다.
			sendHeartbeat()
			for {
				select {
				case <-ticker.C:
					sendHeartbeat()
				case <-done:
					return
				}
			}
		}()
	}
	fmt.Println(&s.RootPath)
	s.Start()
	return 0
}

func resolveDataDir() string {
	// Figure out what data directory to tell algod to use.
	// If not specified on cmdline with '-d', look for default in environment.
	// algod에게 사용하도록 지시할 데이터 디렉토리를 파악합니다.
	// cmdline에 '-d'로 지정하지 않으면 환경에서 기본값을 찾습니다.
	var dir string
	if dataDirectory == nil || *dataDirectory == "" {
		dir = os.Getenv("ALGORAND_DATA")
	} else {
		dir = *dataDirectory
	}
	return dir
}
