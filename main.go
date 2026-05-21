package main

import (
	"fmt"
	"time"

	argparse "github.com/rsa17826/go-arg-lib"
	input "github.com/rsa17826/go-input-lib"
	"github.com/rsa17826/input-manager/IMan"
)

func infoEvent(code uint16, val int32) IMan.WireEvent {
	t := time.Now()
	return IMan.WireEvent{
		Sec:   t.Unix(),
		Usec:  int64(t.Nanosecond() / 1000),
		Type:  input.EV_KEY,
		Code:  code,
		Value: val,
	}
}

func main() {
	argparse.ParseArgs([]argparse.ArgumentData{})

	// Fire up all three listening channels concurrently in one call
	im, err := IMan.Connect(IMan.ModeInjection, IMan.ModeBlocking, IMan.ModeListen, IMan.ModeVirtListen)
	if err != nil {
		panic(err)
	}
	defer im.Close()

	fmt.Println("All connections pooled into a single processing loop successfully.")

	// ONE SINGLE LOOP handles every incoming read request!
	go func() {
		for {
			re, err := im.ReadNext()
			if err != nil {
				fmt.Println("Unified reader stream broken:", err)
				return
			}

			switch re.From {
			case IMan.ModeListen:
				fmt.Printf("[REAL KBD/MOUSE LOG] Code: %d, Val: %d\n", re.Event.Code, re.Event.Value)

			case IMan.ModeVirtListen:
				fmt.Printf("[VIRTUAL SUBSYSTEM LOG] Code: %d, Val: %d\n", re.Event.Code, re.Event.Value)

			case IMan.ModeBlocking:
				if re.Event.Code == input.KEY_A {
					im.BlockInput(1) // Block the "A" key completely!
					fmt.Println("[BLOCKED] Stopped key 'A' from typing.")
				} else {
					im.BlockInput(0) // Let everything else pass through
				}
			}
		}
	}()

	// Periodic Event Injection Generator Loop
	go func() {
		for {
			time.Sleep(5 * time.Second)
			fmt.Println("Injecting KEY_0 DOWN...")
			im.Send(infoEvent(input.KEY_0, 1))

			time.Sleep(5 * time.Second)
			fmt.Println("Injecting KEY_0 UP...")
			im.Send(infoEvent(input.KEY_0, 0))
		}
	}()

	select {}
}
