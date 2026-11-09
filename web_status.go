package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type printerModule struct {
	Key      byte   `json:"key"`
	ID       uint16 `json:"id"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Firmware string `json:"firmware,omitempty"`
}

type printerNozzle struct {
	Module         byte     `json:"module"`
	Index          byte     `json:"index"`
	Type           *byte    `json:"type,omitempty"`
	Diameter       *float32 `json:"diameter,omitempty"`
	DiameterSource string   `json:"diameterSource,omitempty"`
	Temperature    *float32 `json:"temperature,omitempty"`
	Target         *float32 `json:"target,omitempty"`
	Active         *bool    `json:"active,omitempty"`
}

type printerBed struct {
	Module      byte    `json:"module"`
	Zone        byte    `json:"zone"`
	Temperature float32 `json:"temperature"`
	Target      int16   `json:"target"`
}

type printerStatus struct {
	ID       string          `json:"id"`
	IP       string          `json:"ip"`
	Model    string          `json:"model"`
	Protocol string          `json:"protocol"`
	State    string          `json:"state"`
	Modules  []printerModule `json:"modules"`
	Nozzles  []printerNozzle `json:"nozzles"`
	Beds     []printerBed    `json:"beds"`
	Details  json.RawMessage `json:"details,omitempty"`
	Warnings []string        `json:"warnings"`
}

func parseSACPBeds(data []byte) ([]printerBed, error) {
	if len(data) < 2 || len(data) < 2+int(data[1])*7 {
		return nil, errInvalidSize
	}
	beds := make([]printerBed, 0, data[1])
	for index := 0; index < int(data[1]); index++ {
		entry := data[2+index*7 : 2+(index+1)*7]
		beds = append(beds, printerBed{Module: data[0], Zone: entry[0], Temperature: float32(int32(binary.LittleEndian.Uint32(entry[1:5]))) / 1000, Target: int16(binary.LittleEndian.Uint16(entry[5:7]))})
	}
	return beds, nil
}

func sacpWorkflow(conn net.Conn) (string, error) {
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	packet := SACP_pack{ReceiverID: 1, Sequence: 500, CommandSet: 1, CommandID: 0, Data: []byte{1, 0xa0, 0xe8, 0x03}}
	if _, err := conn.Write(packet.Encode()); err != nil {
		return "", err
	}
	deadline := time.Now().Add(3 * time.Second)
	states := []string{"Idle", "Starting", "Running", "Pausing", "Paused", "Stopping", "Stopped", "Finishing", "Completed", "Recovering", "Resuming"}
	for time.Now().Before(deadline) {
		response, err := SACP_read(conn, time.Until(deadline))
		if err != nil {
			return "", err
		}
		if response.CommandSet == 1 && response.CommandID == 0 && response.Sequence == 500 && (len(response.Data) == 0 || response.Data[0] != 0) {
			return "", fmt.Errorf("workflow subscription rejected")
		}
		if response.CommandSet == 1 && response.CommandID == 0xa0 && len(response.Data) >= 2 && response.Data[0] == 0 {
			if int(response.Data[1]) < len(states) {
				return states[response.Data[1]], nil
			}
			return "Unknown", nil
		}
	}
	return "", fmt.Errorf("workflow status timed out")
}

var moduleNames = map[uint16]string{
	0: "Print head", 1: "CNC head", 2: "Laser head", 5: "Enclosure",
	6: "Rotary module", 7: "Air purifier", 8: "Emergency stop", 13: "Dual extrusion head",
	14: "10 W laser", 15: "High power CNC", 16: "Enclosure", 18: "Dual extrusion head", 19: "20 W laser",
	20: "40 W laser", 23: "2 W laser", 512: "Heated bed", 513: "J1 heated bed",
	514: "J1 linear module", 515: "Artisan heated bed", 517: "Emergency stop", 518: "Enclosure",
	519: "Quick Swap Kit", 522: "Bracing Kit",
}

func valuePointer[T any](value T) *T {
	return &value
}

func isPrintModule(id uint16) bool {
	return id == 0 || id == 13 || id == 18
}

func applyLubanNozzleProfiles(configDir, model string, modules []printerModule, nozzles []printerNozzle) {
	model = strings.ToLower(model)
	series := ""
	for _, candidate := range []string{"a150", "a250", "a350"} {
		if strings.Contains(model, candidate) {
			series = candidate
			break
		}
	}
	if series == "" {
		return
	}
	for index := range nozzles {
		nozzle := &nozzles[index]
		if nozzle.Diameter != nil || nozzle.Index > 1 {
			continue
		}
		profile := ""
		for _, module := range modules {
			if module.Key == nozzle.Module && isPrintModule(module.ID) {
				profile = series + "_single"
				if module.ID == 13 || module.ID == 18 {
					profile = series + "_dual"
				}
				break
			}
		}
		if profile == "" {
			continue
		}
		path := filepath.Join(configDir, "snapmaker-luban", "Config", "printing", profile, fmt.Sprintf("extruder_%d.def.json", nozzle.Index))
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		var definition struct {
			Overrides struct {
				NozzleSize struct {
					DefaultValue *float32 `json:"default_value"`
				} `json:"machine_nozzle_size"`
			} `json:"overrides"`
		}
		err = json.NewDecoder(io.LimitReader(file, 1<<20)).Decode(&definition)
		file.Close()
		diameter := definition.Overrides.NozzleSize.DefaultValue
		if err == nil && diameter != nil && *diameter > 0 && *diameter <= 2.5 {
			nozzle.Diameter = diameter
			nozzle.DiameterSource = "Luban profile"
		}
	}
}

func parseHTTPModuleInfo(data []byte, modules []printerModule) ([]printerNozzle, []printerBed, error) {
	var result struct {
		ModuleInfo []struct {
			Key                      byte     `json:"key"`
			NozzleTemperature        *float32 `json:"nozzleTemperature"`
			NozzleTargetTemperature  *float32 `json:"nozzleTargetTemperature"`
			NozzleTemperature1       *float32 `json:"nozzleTemperature1"`
			NozzleTargetTemperature1 *float32 `json:"nozzleTargetTemperature1"`
			NozzleTemperature2       *float32 `json:"nozzleTemperature2"`
			NozzleTargetTemperature2 *float32 `json:"nozzleTargetTemperature2"`
			HeatedBedTemperature     *float32 `json:"heatedBedTemperature"`
			HeatedBedTarget          *int16   `json:"heatedBedTargetTemperature"`
		} `json:"moduleInfo"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, nil, err
	}
	var nozzles []printerNozzle
	var beds []printerBed
	for _, info := range result.ModuleInfo {
		var module *printerModule
		for index := range modules {
			if modules[index].Key == info.Key {
				module = &modules[index]
				break
			}
		}
		if module != nil && isPrintModule(module.ID) {
			left := printerNozzle{Module: info.Key, Index: 0, Temperature: info.NozzleTemperature1, Target: info.NozzleTargetTemperature1}
			if left.Temperature == nil {
				left.Temperature = info.NozzleTemperature
			}
			if left.Target == nil {
				left.Target = info.NozzleTargetTemperature
			}
			nozzles = append(nozzles, left)
			if module.ID == 13 || module.ID == 18 || info.NozzleTemperature2 != nil || info.NozzleTargetTemperature2 != nil {
				nozzles = append(nozzles, printerNozzle{Module: info.Key, Index: 1, Temperature: info.NozzleTemperature2, Target: info.NozzleTargetTemperature2})
			}
		}
		if info.HeatedBedTemperature != nil && info.HeatedBedTarget != nil {
			beds = append(beds, printerBed{Module: info.Key, Zone: 0, Temperature: *info.HeatedBedTemperature, Target: *info.HeatedBedTarget})
		}
	}
	return nozzles, beds, nil
}

func setHTTPActiveNozzle(data []byte, nozzles []printerNozzle) error {
	var result struct {
		Active *int `json:"active"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}
	if result.Active == nil || *result.Active < 0 || *result.Active > 1 {
		return fmt.Errorf("invalid active extruder")
	}
	for index := range nozzles {
		nozzles[index].Active = valuePointer(int(nozzles[index].Index) == *result.Active)
	}
	return nil
}

func moduleName(id uint16) string {
	if name, ok := moduleNames[id]; ok {
		return name
	}
	return fmt.Sprintf("Module %d", id)
}

func parseSACPModules(data []byte) ([]printerModule, error) {
	if len(data) < 1 {
		return nil, errInvalidSize
	}
	modules := make([]printerModule, 0, data[0])
	data = data[1:]
	states := []string{"Ready", "Upgrading", "Unavailable", "Upgrade failed", "Other"}
	for index := 0; index < cap(modules); index++ {
		if len(data) < 12 {
			return nil, errInvalidSize
		}
		length := int(binary.LittleEndian.Uint16(data[10:12]))
		if len(data) < 12+length {
			return nil, errInvalidSize
		}
		id := binary.LittleEndian.Uint16(data[1:3])
		state := "Unknown"
		if int(data[4]) < len(states) {
			state = states[data[4]]
		}
		modules = append(modules, printerModule{Key: data[0], ID: id, Name: moduleName(id), State: state, Firmware: string(data[12 : 12+length])})
		data = data[12+length:]
	}
	return modules, nil
}

func parseSACPNozzles(data []byte) ([]printerNozzle, error) {
	if len(data) < 4 || len(data) < 4+int(data[3])*17 {
		return nil, errInvalidSize
	}
	nozzles := make([]printerNozzle, 0, data[3])
	for index := 0; index < int(data[3]); index++ {
		entry := data[4+index*17 : 4+(index+1)*17]
		diameter := float32(int32(binary.LittleEndian.Uint32(entry[5:9]))) / 1000
		temperature := float32(int32(binary.LittleEndian.Uint32(entry[9:13]))) / 1000
		nozzles = append(nozzles, printerNozzle{Module: data[0], Index: entry[0], Type: valuePointer(entry[4]), Diameter: valuePointer(diameter), Temperature: valuePointer(temperature), Target: valuePointer(float32(binary.LittleEndian.Uint16(entry[13:15]))), Active: valuePointer(entry[3] == 1)})
	}
	return nozzles, nil
}

func sacpQuery(conn net.Conn, sequence uint16, commandSet, commandID byte, data []byte) ([]byte, error) {
	conn.SetWriteDeadline(time.Now().Add(SACPTimeout * time.Second))
	packet := SACP_pack{ReceiverID: 1, SenderID: 0, Sequence: sequence, CommandSet: commandSet, CommandID: commandID, Data: data}
	if _, err := conn.Write(packet.Encode()); err != nil {
		return nil, err
	}
	for {
		response, err := SACP_read(conn, SACPTimeout*time.Second)
		if err != nil {
			return nil, err
		}
		if response.Sequence == sequence && response.CommandSet == commandSet && response.CommandID == commandID {
			if len(response.Data) == 0 {
				return nil, errInvalidSize
			}
			if response.Data[0] != 0 {
				return nil, fmt.Errorf("printer returned error %d", response.Data[0])
			}
			return response.Data[1:], nil
		}
	}
}

func readPrinterStatus(printer *Printer) (printerStatus, error) {
	Connector.mu.Lock()
	defer Connector.mu.Unlock()
	status := printerStatus{ID: printer.ID, IP: printer.IP, Model: printer.Model, State: "Unknown", Modules: []printerModule{}, Nozzles: []printerNozzle{}, Beds: []printerBed{}, Warnings: []string{}}
	for _, handler := range Connector.handlers {
		if !handler.Ping(printer) {
			continue
		}
		if err := handler.Connect(); err != nil {
			return status, err
		}
		defer handler.Disconnect()
		if OnPrinterUpdate != nil {
			OnPrinterUpdate(printer)
		}
		status.Model = printer.Model
		switch connection := handler.(type) {
		case *SACPConnector:
			status.Protocol = "SACP"
			data, err := sacpQuery(connection.conn, 10, 0x01, 0x20, nil)
			if err != nil {
				return status, err
			}
			status.Modules, err = parseSACPModules(data)
			if err != nil {
				return status, err
			}
			for index, module := range status.Modules {
				if module.ID == 512 || module.ID == 513 || module.ID == 515 {
					data, err = sacpQuery(connection.conn, uint16(11+index), 0x14, 0x01, []byte{module.Key})
					if err == nil {
						var beds []printerBed
						beds, err = parseSACPBeds(data)
						status.Beds = append(status.Beds, beds...)
					}
					if err != nil {
						status.Warnings = append(status.Warnings, fmt.Sprintf("Bed information: %v", err))
					}
					continue
				}
				if !isPrintModule(module.ID) {
					continue
				}
				data, err = sacpQuery(connection.conn, uint16(11+index), 0x10, 0x01, []byte{module.Key})
				if err == nil {
					var nozzles []printerNozzle
					nozzles, err = parseSACPNozzles(data)
					status.Nozzles = append(status.Nozzles, nozzles...)
				}
				if err != nil {
					status.Warnings = append(status.Warnings, fmt.Sprintf("Nozzle information for %s: %v", module.Name, err))
				}
			}
			if workflow, err := sacpWorkflow(connection.conn); err == nil {
				status.State = workflow
			}
		case *HTTPConnector:
			status.Protocol = "HTTP"
			client := &http.Client{Timeout: 5 * time.Second}
			for _, endpoint := range []string{"module_list", "module_info", "status", "active_extruder"} {
				if endpoint == "active_extruder" && len(status.Nozzles) == 0 {
					continue
				}
				address := connection.URL("/"+endpoint) + "?token=" + url.QueryEscape(printer.Token)
				response, err := client.Get(address)
				if err != nil {
					status.Warnings = append(status.Warnings, endpoint+": "+err.Error())
					continue
				}
				body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
				response.Body.Close()
				if err != nil || response.StatusCode != http.StatusOK || !json.Valid(body) {
					status.Warnings = append(status.Warnings, endpoint+": unavailable")
					continue
				}
				if endpoint == "module_list" {
					var result struct {
						ModuleList []struct {
							Key      byte   `json:"key"`
							ModuleID uint16 `json:"moduleId"`
							Status   bool   `json:"status"`
						} `json:"moduleList"`
					}
					if json.Unmarshal(body, &result) == nil {
						for _, module := range result.ModuleList {
							state := "Unavailable"
							if module.Status {
								state = "Ready"
							}
							status.Modules = append(status.Modules, printerModule{Key: module.Key, ID: module.ModuleID, Name: moduleName(module.ModuleID), State: state})
						}
					}
				} else if endpoint == "module_info" {
					status.Details = body
					nozzles, beds, err := parseHTTPModuleInfo(body, status.Modules)
					if err != nil {
						status.Warnings = append(status.Warnings, "module_info: invalid response")
					} else {
						status.Nozzles = append(status.Nozzles, nozzles...)
						status.Beds = append(status.Beds, beds...)
					}
				} else if endpoint == "active_extruder" {
					if err := setHTTPActiveNozzle(body, status.Nozzles); err != nil {
						status.Warnings = append(status.Warnings, "active_extruder: invalid response")
					}
				} else {
					var heartbeat struct {
						Status string `json:"status"`
					}
					if json.Unmarshal(body, &heartbeat) == nil && heartbeat.Status != "" {
						status.State = heartbeat.Status
					}
				}
			}
			if configDir, err := os.UserConfigDir(); err == nil {
				applyLubanNozzleProfiles(configDir, status.Model, status.Modules, status.Nozzles)
			}
			for _, module := range status.Modules {
				if isPrintModule(module.ID) {
					status.Warnings = append(status.Warnings, "Nozzle diameter and type are not reported by the printer's HTTP API. Luban profile sizes, if shown, are slicer settings and may differ from the installed nozzles.")
					break
				}
			}
		default:
			return status, fmt.Errorf("status is not supported by this printer protocol")
		}
		return status, nil
	}
	return status, fmt.Errorf("printer %s is not available", printer.IP)
}
