package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/cernops/golbd/lbcluster"
	"io"
	"io/ioutil"
	"log/syslog"
	"math/rand"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var versionFlag = flag.Bool("version", false, "print lbd version and exit")
var debugFlag = flag.Bool("debug", false, "set lbd in debug mode")
var startFlag = flag.Bool("start", false, "start lbd")
var stopFlag = flag.Bool("stop", false, "stop lbd")
var updateFlag = flag.Bool("update", false, "update lbd config")
var configFileFlag = flag.String("config", "./load-balancing.conf", "specify configuration file path")
var logFileFlag = flag.String("log", "./lbd.log", "specify log file path")
var stdoutFlag = flag.Bool("stdout", false, "send log to stdtout")

const itCSgroupDNSserver string = "cfmgr.cern.ch"

type Config struct {
	Master          string
	HeartbeatFile   string
	HeartbeatPath   string
	HeartbeatMu     sync.Mutex
	TsigKeyPrefix   string
	TsigInternalKey string
	TsigExternalKey string
	SnmpPassword    string
	DnsManager      string
	Clusters        map[string][]string
	Parameters      map[string]lbcluster.Params
}

// Read a whole file into the memory and store it as array of lines
func readLines(path string) (lines []string, err error) {
	var (
		file   *os.File
		part   []byte
		prefix bool
	)
	if file, err = os.Open(path); err != nil {
		return
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	buffer := bytes.NewBuffer(make([]byte, 0))
	for {
		if part, prefix, err = reader.ReadLine(); err != nil {
			break
		}
		buffer.Write(part)
		if !prefix {
			lines = append(lines, buffer.String())
			buffer.Reset()
		}
	}
	if err == io.EOF {
		err = nil
	}
	return
}

func loadClusters(config *Config, lg *lbcluster.Log) []lbcluster.LBCluster {
	var hm map[string]int
	var lbc lbcluster.LBCluster
	var lbcs []lbcluster.LBCluster

	for k, v := range config.Clusters {
		if len(v) == 0 {
			lg.Warning("cluster: " + k + " ignored as it has no members defined in the configuration file " + *configFileFlag)
			continue
		}
		if par, ok := config.Parameters[k]; ok {
			logfileDirs := strings.Split(*logFileFlag, "/")
			logfilePath := strings.Join(logfileDirs[:len(logfileDirs)-1], "/")
			lbc = lbcluster.LBCluster{Cluster_name: k, Loadbalancing_username: "loadbalancing",
				Loadbalancing_password: config.SnmpPassword, Parameters: par,
				Current_best_hosts:      []string{"unknown"},
				Previous_best_hosts:     []string{"unknown"},
				Previous_best_hosts_dns: []string{"unknown"},
				Slog:                 lg,
				Statistics_filename:  logfilePath + "/golbstatistics." + k,
				Per_cluster_filename: logfilePath + "/cluster/" + k + ".log"}
			hm = make(map[string]int)
			for _, h := range v {
				hm[h] = lbcluster.WorstValue + 1
			}
			lbc.Host_metric_table = hm
			lbcs = append(lbcs, lbc)
			lbc.Write_to_log("INFO", "(re-)loaded cluster ")

		} else {
			lg.Warning("cluster: " + k + " missing parameters for cluster; ignoring the cluster, please check the configuration file " + *configFileFlag)
		}
	}
	return lbcs

}

func loadConfig(configFile string, lg *lbcluster.Log) (*Config, error) {
	var config Config
	var p lbcluster.Params
	var mc map[string][]string
	mc = make(map[string][]string)
	var mp map[string]lbcluster.Params
	mp = make(map[string]lbcluster.Params)

	lines, err := readLines(configFile)
	if err != nil {
		return &config, err
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "#") || (line == "") {
			continue
		}
		words := strings.Split(line, " ")
		if words[1] == "=" {
			switch words[0] {
			case "master":
				config.Master = words[2]
			case "heartbeat_path":
				config.HeartbeatPath = words[2]
			case "heartbeat_file":
				config.HeartbeatFile = words[2]
			case "tsig_key_prefix":
				config.TsigKeyPrefix = words[2]
			case "tsig_internal_key":
				config.TsigInternalKey = words[2]
			case "tsig_external_key":
				config.TsigExternalKey = words[2]
			case "snmpd_password":
				config.SnmpPassword = words[2]
			case "dns_manager":
				config.DnsManager = words[2]
			}
		} else if words[2] == "=" {
			jsonStream := "{"
			if words[0] == "parameters" {
				for i, param := range words[3:] {
					keyval := strings.Split(param, "#")
					if keyval[1] == "no" {
						jsonStream = jsonStream + strconv.Quote(strings.Title(keyval[0])) + ": false"
					} else if keyval[1] == "yes" {
						jsonStream = jsonStream + strconv.Quote(strings.Title(keyval[0])) + ": true"
					} else if _, err := strconv.Atoi(keyval[1]); err == nil {
						jsonStream = jsonStream + strconv.Quote(strings.Title(keyval[0])) + ": " + keyval[1]
					} else {
						jsonStream = jsonStream + strconv.Quote(strings.Title(keyval[0])) + ": " + strconv.Quote(keyval[1])
					}
					if i < (len(words[3:]) - 1) {
						jsonStream = jsonStream + ", "
					}
				}
				jsonStream = jsonStream + "}"
				dec := json.NewDecoder(strings.NewReader(jsonStream))
				if err := dec.Decode(&p); err == io.EOF {
					break
				} else if err != nil {
					//log.Fatal(err)
					lg.Warning(fmt.Sprintf("%v", err))
					os.Exit(1)
				}
				mp[words[1]] = p

			} else if words[0] == "clusters" {
				mc[words[1]] = words[3:]
				lg.Debug(words[1])
				lg.Debug(fmt.Sprintf("%v", words[3:]))
			}
		}
	}
	config.Parameters = mp
	config.Clusters = mc
	return &config, nil

}

func should_update_dns(config *Config, hostname string, lg *lbcluster.Log) bool {
	if hostname == config.Master {
		return true
	}
	master_heartbeat := "I am sick"
	connectTimeout := (10 * time.Second)
	readWriteTimeout := (20 * time.Second)
	httpClient := lbcluster.NewTimeoutClient(connectTimeout, readWriteTimeout)
	response, err := httpClient.Get("http://" + config.Master + "/load-balancing/" + config.HeartbeatFile)
	if err != nil {
		lg.Warning(fmt.Sprintf("problem fetching heartbeat file from the primary master %v: %v", config.Master, err))
		return true
	} else {
		defer response.Body.Close()
		contents, err := ioutil.ReadAll(response.Body)
		if err != nil {
			lg.Warning(fmt.Sprintf("%s", err))
		}
		lg.Debug(fmt.Sprintf("%s", contents))
		master_heartbeat = strings.TrimSpace(string(contents))
		lg.Info("primary master heartbeat: " + master_heartbeat)
		r, _ := regexp.Compile(config.Master + ` : (\d+) : I am alive`)
		if r.MatchString(master_heartbeat) {
			matches := r.FindStringSubmatch(master_heartbeat)
			lg.Debug(fmt.Sprintf(matches[1]))
			if mastersecs, err := strconv.ParseInt(matches[1], 10, 64); err == nil {
				now := time.Now()
				localsecs := now.Unix()
				diff := localsecs - mastersecs
				lg.Info(fmt.Sprintf("primary master heartbeat time difference: %v seconds", diff))
				if diff > 600 {
					return true
				}
			}
		} else {
			// Upload - heartbeat has unexpected values
			return true
		}
		// Do not upload, heartbeat was OK
		return false
	}
}

func update_heartbeat(config *Config, hostname string, lg *lbcluster.Log) error {
	if hostname != config.Master {
		return nil
	}
	heartbeat_file := config.HeartbeatPath + "/" + config.HeartbeatFile + "temp"
	heartbeat_file_real := config.HeartbeatPath + "/" + config.HeartbeatFile

	config.HeartbeatMu.Lock()
	defer config.HeartbeatMu.Unlock()

	f, err := os.OpenFile(heartbeat_file, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		lg.Error(fmt.Sprintf("can not open %v for writing: %v", heartbeat_file, err))
		return err
	}
	now := time.Now()
	secs := now.Unix()
	_, err = fmt.Fprintf(f, "%v : %v : I am alive\n", hostname, secs)
	lg.Info("updating: heartbeat file " + heartbeat_file)
	if err != nil {
		lg.Info(fmt.Sprintf("can not write to %v: %v", heartbeat_file, err))
	}
	f.Close()
	if err = os.Rename(heartbeat_file, heartbeat_file_real); err != nil {
		lg.Error(fmt.Sprintf("can not rename %v to %v: %v", heartbeat_file, heartbeat_file_real, err))
		return err
	}
	return nil
}

func installSignalHandler(sighup, sigterm *bool, lg *lbcluster.Log) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		for {
			// Block until a signal is received.
			sig := <-c
			lg.Info(fmt.Sprintf("\nGiven signal: %v\n", sig))
			switch sig {
			case syscall.SIGHUP:
				*sighup = true
			case syscall.SIGTERM:
				*sigterm = true
			}
		}
	}()
}

func main() {
	flag.Parse()
	if *versionFlag {
		fmt.Printf("This is a proof of concept golbd version %s \n", "0.001")
		os.Exit(0)
	}
	rand.Seed(time.Now().UTC().UnixNano())
	log, e := syslog.New(syslog.LOG_NOTICE, "lbd")
	lg := lbcluster.Log{Writer: *log, Syslog: false, Stdout: *stdoutFlag, Debugflag: *debugFlag, TofilePath: *logFileFlag}
	if e == nil {
		lg.Info("Starting lbd")
	}

	var sig_hup, sig_term bool
	installSignalHandler(&sig_hup, &sig_term, &lg)

	hostname, e := os.Hostname()
	if e == nil {
		lg.Info("Hostname: " + hostname)
	}

	config, e := loadConfig(*configFileFlag, &lg)
	if e != nil {
		lg.Warning("loadConfig Error: ")
		lg.Warning(e.Error())
		os.Exit(1)
	} else {
		lg.Debug(fmt.Sprintf("config %v", config))
	}

	if *debugFlag {
		for k, v := range config.Parameters {
			lg.Debug(fmt.Sprintf("params %v %v", k, v))
		}
		for k, v := range config.Clusters {
			lg.Debug(fmt.Sprintf("clusters %v %v", k, v))
		}
	}
	lg.Info("Loading clusters")
	lbclusters := loadClusters(config, &lg)
	lg.Info("Clusters loaded")
	var wg sync.WaitGroup
	for {
		lg.Info("Starting the loop")
		if sig_term {
			break
		}
		if sig_hup {
			config, e = loadConfig(*configFileFlag, &lg)
			if e != nil {
				lg.Warning("loadConfig Error: ")
				lg.Warning(e.Error())
				os.Exit(1)
			} else {
				lg.Debug(fmt.Sprintf("%v", config))
			}

			if *debugFlag {
				for k, v := range config.Parameters {
					lg.Debug(fmt.Sprintf("params %v %v", k, v))
				}
				for k, v := range config.Clusters {
					lg.Debug(fmt.Sprintf("clusters %v %v", k, v))
				}
			}
			lbclusters = loadClusters(config, &lg)

			sig_hup = false
		}

		check_update := true
		update_dns := true
		lg.Info("Checking if any of the " + strconv.Itoa(len(lbclusters)) + " clusters needs updating")
		for i := range lbclusters {
			pc := &lbclusters[i]
			pc.Write_to_log("DEBUG", "DO WE HAVE TO UPDATE?")
			if pc.Time_to_refresh() {
				pc.Write_to_log("INFO", "Time to refresh the cluster")
				if check_update {
					check_update = false
					//Let's make one call to check if we have to update the dns (instead of one per alias)
					update_dns = should_update_dns(config, hostname, &lg)
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					pc.Find_best_hosts()
					//pc.Create_statistics()
					if update_dns {
						pc.Write_to_log("DEBUG", "Should update dns is true")
						pc.Refresh_dns(config.DnsManager, config.TsigKeyPrefix, config.TsigInternalKey, config.TsigExternalKey)
					} else {
						pc.Write_to_log("DEBUG", "should_update_dns false")
					}
				}()
			}
		}
		wg.Wait()

		if update_dns {
			update_heartbeat(config, hostname, &lg)
		}

		lg.Debug("iteration done!")
		if !sig_term {
			time.Sleep(10 * time.Second)
		}
	}
	lg.Info("all done!")
	os.Exit(0)
}
