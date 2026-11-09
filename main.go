package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/manifoldco/promptui"
)

var (
	Host                string
	KnownHosts          string
	DiscoverTimeout     time.Duration
	OctoPrintListenAddr string
	WebInterface        bool
	Tool1Temperature    int
	Tool2Temperature    int
	BedTemperature      int
	Home                bool
	Debug               bool

	_Payloads []*Payload
)

func main() {
	defer func() {
		if r := recover(); r != nil {
			os.Exit(2)
		}
	}()

	defaultKnownHosts, defaultPathErr := defaultKnownHostsPath()
	if envKnownhosts := os.Getenv("KNOWN_HOSTS"); envKnownhosts != "" {
		defaultKnownHosts = envKnownhosts
	}

	flag.StringVar(&Host, "host", os.Getenv("HOST"), "upload to host(id/ip/hostname), not required.")
	flag.StringVar(&KnownHosts, "knownhosts", defaultKnownHosts, "known hosts")
	flag.StringVar(&OctoPrintListenAddr, "octoprint", os.Getenv("OCTOPRINT"), "octoprint listen address, e.g. '-octoprint :8844' then you can upload files to printer by http://localhost:8844")
	flag.BoolVar(&WebInterface, "web-interface", false, "start the local web interface on 127.0.0.1:8889")
	flag.IntVar(&Tool1Temperature, "tool1", parseIntEnv("TOOL1", 0), "set the temperature (preheat) of tool 1")
	flag.IntVar(&Tool2Temperature, "tool2", parseIntEnv("TOOL2", 0), "set the temperature (preheat) of tool 2")
	flag.IntVar(&BedTemperature, "bed", parseIntEnv("BED", 0), "set the temperature (preheat) of bed")
	flag.BoolVar(&Home, "home", parseBoolEnv("HOME", false), "home the printer")
	flag.DurationVar(&DiscoverTimeout, "timeout", parseDurationEnv("TIMEOUT", 4*time.Second), "printer discovery timeout")
	flag.BoolVar(&Debug, "debug", parseBoolEnv("DEBUG", false), "debug mode")

	flag.Usage = flag_usage
	flag.Parse()
	if KnownHosts == "" {
		log.Panicf("Cannot determine known hosts path: %v", defaultPathErr)
	}

	if Debug {
		log.Printf("-- Debug mode: %s", Version)
	}

	var printer *Printer
	legacyPath := ""
	if os.Getenv("KNOWN_HOSTS") == "" && KnownHosts == defaultKnownHosts {
		if executable, err := os.Executable(); err == nil {
			legacyPath = filepath.Join(filepath.Dir(executable), "hosts.yaml")
		}
	}
	ls, err := NewLocalStorage(KnownHosts, legacyPath)
	if err != nil {
		log.Panicln(err)
	}
	// Persist the token to known hosts as soon as it is (re)obtained on connect,
	// so it survives restarts even on an unclean exit (the shutdown defer below
	// only runs on a graceful exit).
	OnPrinterUpdate = func(p *Printer) {
		if p == nil {
			return
		}
		ls.Add(p)
		if err := ls.Save(); err != nil {
			log.Printf("Could not save known hosts: %v", err)
		}
	}
	defer func() {
		if printer != nil {
			// update printer's token
			ls.Add(printer)
			if Debug {
				log.Printf("-- Updated printer: %s", printer.String())
			}
		}
		if err := ls.Save(); err == nil && Debug {
			log.Printf("-- Saved known hosts: %s", KnownHosts)
		}
	}()

	// Check if host is specified
	printer = ls.Find(Host)
	if printer != nil {
		log.Println("Found printer in " + KnownHosts)
	}

	// Discover printers
	if printer == nil {
		log.Println("Discovering ...")
		if printers, err := Discover(DiscoverTimeout); err == nil {
			if Debug {
				log.Printf("-- Discovered %d printers", len(printers))
			}
			ls.Add(printers...)
			if err := ls.Save(); err != nil {
				log.Printf("Could not save known hosts: %v", err)
			}
		} else if Debug {
			log.Printf("-- Discover error: %s", err.Error())
		}
		printer = ls.Find(Host)
		if printer != nil {
			log.Printf("Found printer: %s", printer.String())
		}
	}

	if printer == nil {
		if Host == "" {
			// Prompt user to select a printer
			printers := ls.Printers
			if len(printers) == 0 {
				log.Panicln("No printers found")
			}
			if len(printers) > 1 {
				prompt := promptui.Select{
					Label: "Select a printer",
					Items: printers,
				}
				idx, _, err := prompt.Run()
				if err != nil {
					log.Panicln(err)
				}
				printer = printers[idx]
			} else {
				printer = printers[0]
			}
		} else {
			// directly to printer using ip/hostname; keep an ID so the token gets
			// persisted (ls.Add skips ID-less printers)
			printer = &Printer{IP: Host, ID: Host}
		}
	}

	ls.Add(printer)
	if err := ls.Save(); err != nil {
		log.Printf("Could not save known hosts: %v", err)
	}

	log.Println("Printer IP:", printer.IP)
	if printer.Model != "" {
		log.Println("Printer Model:", printer.Model)
	}

	// Create a channel to listen for signals
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		sig := <-sc
		log.Printf("Received signal: %s", sig)
		os.Exit(0)
	}()

	if WebInterface {
		if OctoPrintListenAddr != "" {
			log.Panicln("-web-interface and -octoprint cannot be used together")
		}
		if err := startWebInterface(printer); err != nil {
			log.Panic(err)
		}
		return
	}

	if OctoPrintListenAddr != "" {
		// listen for octoprint uploads
		if err := startOctoPrintServer(OctoPrintListenAddr, printer); err != nil {
			log.Panic(err)
		}
		return
	}

	preheating := Tool1Temperature != 0 || Tool2Temperature != 0 || BedTemperature != 0 || Home
	if preheating {
		log.Println("Preheating...")
		if err := Connector.PreHeatCommands(printer, Tool1Temperature, Tool2Temperature, BedTemperature, Home); err != nil {
			log.Panic(err)
		}
	}

	// 检查文件参数是否存在 - Check if the file parameter exists
	for _, file := range flag.Args() {
		st, err := os.Stat(file)
		if err != nil {
			log.Panicf("Cannot stat %s: %v", file, err)
		}
		f, err := os.Open(file)
		if err != nil {
			log.Panicf("Cannot open %s: %v", file, err)
		}
		defer f.Close()
		_Payloads = append(_Payloads, NewPayload(f, st.Name(), st.Size()))
	}

	// 检查是否有传入的文件 - Check if a file has been passed in
	if len(_Payloads) == 0 {
		if !preheating {
			log.Panicln("No input files")
		}
	}

	// 从 slic3r 环境变量中获取文件名
	envFilename := os.Getenv("SLIC3R_PP_OUTPUT_NAME")

	// Upload files to host
	for _, p := range _Payloads {
		if envFilename != "" {
			p.SetName(filepath.Base(envFilename))
		}

		log.Printf("Uploading file '%s' [%s]...", p.Name, p.ReadableSize())
		if err := Connector.Upload(printer, p); err != nil {
			log.Panicln(err)
		} else {
			log.Println("Upload finished.")
			<-time.After(time.Second * 1) // HMI needs some time to refresh
		}
	}
}
