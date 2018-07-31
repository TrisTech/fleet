package fleet

import (
	"io"
	"log"
	"os"

	"github.com/MagicalTux/ringbuf"
)

var logbuf *ringbuf.Writer

func init() {
	var err error

	logbuf, err = ringbuf.New(1024 * 1024)
	if err == nil {
		log.SetOutput(logbuf)
		go io.Copy(os.Stdout, logbuf.BlockingReader())
	} else {
		log.Printf("[fleet] Failed to setup logbuf: %s", err)
	}
}

func LogTarget() io.Writer {
	return logbuf
}

func LogDmesg(w io.Writer) (int64, error) {
	return io.Copy(w, logbuf.Reader())
}
