package logger

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/nxadm/tail"
)

// StartTailer follows the log from its current end.
func StartTailer(logPath string, linesChan chan<- string) error {
	return StartTailerAt(logPath, -1, linesChan)
}

// StartTailerAt opens the log and sends incoming lines down the channel.
// offset >= 0 begins reading at that byte position (used to continue exactly
// where an initial scan stopped); a negative offset starts at end-of-file.
func StartTailerAt(logPath string, offset int64, linesChan chan<- string) error {
	// If the file doesn't exist yet, wait or poll until it does
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		fmt.Printf("[LOGGER] Log file not found yet at %s. Waiting for MTG Arena to create it...\n", logPath)
		for {
			time.Sleep(1 * time.Second)
			if _, err := os.Stat(logPath); err == nil {
				break
			}
		}
	}

	seek := &tail.SeekInfo{Offset: 0, Whence: io.SeekEnd}
	if offset >= 0 {
		seek = &tail.SeekInfo{Offset: offset, Whence: io.SeekStart}
	}

	config := tail.Config{
		Follow:    true,  // Continuously follow new appends
		ReOpen:    true,  // Re-opens the file if MTG Arena truncates or recreates it
		MustExist: false, // Don't crash if the file disappears during a client restart
		Poll:      true,  // Polling mode for compatibility under Linux Proton/Wine shares
		Location:  seek,
	}

	t, err := tail.TailFile(logPath, config)
	if err != nil {
		return fmt.Errorf("failed to tail log file: %w", err)
	}
	defer t.Cleanup()

	fmt.Printf("[LOGGER] Tailer actively monitoring: %s (offset %d)\n", logPath, offset)

	for line := range t.Lines {
		if line.Err != nil {
			fmt.Printf("[LOGGER WARN] Tail error: %v\n", line.Err)
			continue
		}
		if line.Text != "" {
			linesChan <- line.Text
		}
	}

	return nil
}
