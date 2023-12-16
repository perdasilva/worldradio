package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	// rtmp server
	rtmpServer := "rtmp://localhost/live/stream"

	if len(os.Args) == 2 {
		rtmpServer = os.Args[1]
	}

	// storage locations
	const opusRecordingDir = "./opus"
	const rawRecordingDir = "./raw"

	// queues/channels for interprocess comms
	const fifoPath = "./audio-fifo"
	opusRecs := make(chan string, 100)
	rawRecs := make(chan string, 100)

	// create a context to make sure we shutdown the go routines
	// upon exit
	ctx, cancel := context.WithCancel(context.Background())

	quit := make(chan os.Signal)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	done := make(chan struct{})
	// start up the system
	go func() {
		slog.Info("starting system")
		errGrp, errGrpCtx := errgroup.WithContext(ctx)
		// server collects opus recordings from the frontend
		errGrp.Go(server(errGrpCtx, opusRecordingDir, opusRecs))

		// decoder converts opus to pcm
		errGrp.Go(decoder(errGrpCtx, rawRecordingDir, opusRecs, rawRecs))

		// broadcaster pipes pcm to the broadcaster which sends it off to the rtmp server
		errGrp.Go(broadcast(errGrpCtx, rtmpServer, fifoPath, rawRecordingDir, rawRecs))

		if err := errGrp.Wait(); err != nil {
			slog.Warn("system failure", "error", err)
		}
		slog.Info("system shutdown")
		close(done)
	}()

	// wait for termination signal and clean up
	<-quit
	cancel()
	<-done
}

func broadcast(ctx context.Context, rtmpServer string, fifoPath string, rawRecDir string, rawRecChan <-chan string) func() error {
	return func() error {
		// create rawRecDir if necessary
		if err := os.MkdirAll(rawRecDir, 0755); err != nil {
			return err
		}

		// bring broadcaster back if any of its processes fail (limit to 5 retries)
		for retries := 5; retries > 0; retries-- {
			select {
			case <-ctx.Done():
				slog.Info("broadcast shutting down")
				return nil
			default:
				slog.Info("starting broadcast")
				// delete fifo on start
				if err := os.Remove(fifoPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}

				// create new fifo
				if err := unix.Mkfifo(fifoPath, 0640); err != nil {
					slog.Warn("broadcast failure", "error", err)
					return err
				}

				fifo, err := os.OpenFile(fifoPath, os.O_RDWR, os.ModeNamedPipe)
				if err != nil {
					slog.Warn("broadcast failure", "error", err)
					return err
				}

				// start processes
				errGrp, errGrpCtx := errgroup.WithContext(ctx)
				errGrp.Go(streamBuilder(errGrpCtx, fifo, rawRecChan))
				errGrp.Go(broadcaster(errGrpCtx, rtmpServer, fifoPath))

				slog.Info("broadcast started")
				if err := errGrp.Wait(); err != nil {
					slog.Warn("broadcast failure", "error", err)
				}

				// clean up
				slog.Info("cleaning up broadcast")
				if err := fifo.Close(); err != nil {
					slog.Warn("broadcast failure", "error", err)
				}
				if err := os.Remove(fifoPath); err != nil {
					slog.Warn("broadcast failure", "error", err)
				}
			}
		}
		return fmt.Errorf("complete broadcast failure after retries exhausted")
	}
}

func broadcaster(ctx context.Context, rtmpServer, fifoPath string) func() error {
	return func() error {
		slog.Info("starting broadcaster")
		ffmpegCommand := []string{
			"-f", "s16le",
			"-ar", "48000",
			"-ac", "1",
			"-i", fifoPath,
			"-loop", "1",
			"-i", "./res/worldradio.png",
			"-c:v", "libx264",
			"-preset", "veryfast",
			"-pix_fmt", "yuv420p",
			"-c:a", "aac",
			"-b:a", "64k",
			"-f", "flv",
			rtmpServer,
		}

		cmd := exec.CommandContext(ctx, "ffmpeg", ffmpegCommand...)
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				// The command did not exit cleanly
				if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
					if status.Signaled() {
						// Process was terminated by a signal
						if status.Signal() == syscall.SIGKILL {
							slog.Info("broadcaster shutdown")
							return nil
						}
					}
				}
			}
			slog.Warn("broadcaster failure", "error", err)
			return err
		}
		return nil
	}
}

func streamBuilder(ctx context.Context, fifo *os.File, rawRecChan <-chan string) func() error {
	return func() error {
		slog.Info("starting streamBuilder")
		// make some noise for the periods sans recording
		const noiseDurationSeconds = 1
		noise := generateWhiteNoise(noiseDurationSeconds)

		for {
			select {
			case <-ctx.Done():
				slog.Info("streamBuilder shutdown")
				return nil
			case rec, ok := <-rawRecChan:
				if !ok {
					return nil
				}

				// dump decoded data to fifo
				f, err := os.Open(rec)
				if err != nil {
					slog.Warn("broadcast failed", "recording", rec, "error", err)
					continue
				}
				s := time.Now()
				n, err := io.Copy(fifo, f)
				e := time.Now().Sub(s)
				_ = f.Close()
				if err != nil {
					slog.Warn("broadcast failed", "recording", rec, "error", err)
					continue
				}
				if err := os.Remove(rec); err != nil {
					slog.Warn("error deleting raw rec", "recording", rec, "error", err)
				}
				slog.Info("broadcast", "recording", rec, "size", n, "time", e)
			default:
				if _, err := fifo.Write(noise); err != nil {
					slog.Warn("broadcast failed", "recording", "noise", "error", err)
				}

				// wait noise duration seconds before dumping more noise
				time.Sleep(noiseDurationSeconds * time.Second)
			}
		}
	}
}

func decoder(ctx context.Context, rawRecDir string, newRecChan <-chan string, rawRecChan chan<- string) func() error {
	return func() error {
		// create rawRecDir if needed
		if err := os.MkdirAll(rawRecDir, 0755); err != nil {
			return err
		}
		slog.Info("starting decoder")
		for {
			select {
			case <-ctx.Done():
				slog.Info("decoder shutdown")
				return nil
			case rec, ok := <-newRecChan:
				if !ok {
					slog.Warn("decoder bailing", "reason", "recording channel closed")
					return nil
				}

				// decode
				s := time.Now()
				decodedFile, err := decodeOpus(rec, rawRecDir)
				e := time.Now().Sub(s)
				if err != nil {
					slog.Warn("decoder failure", "recording", rec, "error", err)
					continue
				}

				// pipe decoded file to new recording channel
				select {
				case <-ctx.Done():
					slog.Warn("decoder bailing", "reason", "context is done")
					return nil
				case rawRecChan <- decodedFile:
				}
				slog.Info("decoder success", "recording", rec, "time", e)
			}
		}
	}
}

func server(ctx context.Context, recordingDir string, newRecChan chan string) func() error {
	return func() error {
		// create rawRecDir if needed
		if err := os.MkdirAll(recordingDir, 0755); err != nil {
			return err
		}
		// Endpoint for uploading audio
		router := gin.Default()
		gin.SetMode(gin.ReleaseMode)
		router.Static("", "./frontend")
		router.POST("/upload-audio", func(c *gin.Context) {
			// Create a file to save the uploaded content
			recordingPath := fmt.Sprintf("%s/%s.ogg", recordingDir, uuid.NewString())
			outFile, err := os.Create(recordingPath)
			if err != nil {
				c.JSON(500, gin.H{"message": "Could not create file"})
				slog.Error("server failure", "error", err)
				return
			}
			defer outFile.Close()

			// Copy the uploaded file to the destination file
			s := time.Now()
			n, err := io.Copy(outFile, c.Request.Body)
			e := time.Now().Sub(s)
			if err != nil {
				slog.Error("server failure", "error", err, "size", n)
				c.JSON(500, gin.H{"message": "Failed to save file"})
				return
			}

			// try to enqueue recording and fail after 3 seconds
			timeout := time.After(3 * time.Second)
			select {
			case newRecChan <- recordingPath:
				slog.Info("server success", "recording", recordingPath, "time", e)
				c.JSON(200, gin.H{"message": "File uploaded successfully"})
			case <-timeout:
				slog.Error("server failure", "error", "queue is backed up")
				c.JSON(429, gin.H{"message:": "Too many requests"})
			}
		})

		srv := &http.Server{
			Addr:    ":8080",
			Handler: router,
		}

		// start server in another routine
		go func() {
			// service connections
			slog.Info("starting server")
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Warn("server failure", "error", err)
			}
		}()

		// wait for context to finish or server to fail
		select {
		case <-ctx.Done():
			// attempt graceful shutdown of the server
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				slog.Warn("server shutdown error", "error", err)
			}
		}
		slog.Info("server shutdown")
		return nil
	}
}

func generateWhiteNoise(durationSeconds int) []byte {
	// generate random pcm data
	slog.Info("Generating random noise")
	numSamples := durationSeconds * 48000
	pcm := make([]byte, numSamples*2)
	for i := 0; i < numSamples; i++ {
		pcmData := uint16(rand.Intn(1000) - 500)
		binary.LittleEndian.PutUint16(pcm[i*2:], pcmData)
	}
	return pcm
}

func decodeOpus(opusFile string, outputDir string) (string, error) {
	stat, err := os.Stat(opusFile)
	if err != nil {
		return "", err
	} else if stat.IsDir() {
		return "", fmt.Errorf("error: '%s' is a directory and not a file", opusFile)
	}

	outputFile := filepath.Join(
		outputDir,
		fmt.Sprintf("%s.wav", strings.TrimSuffix(stat.Name(), filepath.Ext(stat.Name()))))
	decodeCmd := []string{
		"ffmpeg",
		"-i", opusFile,
		"-acodec", "pcm_s16le",
		outputFile,
	}
	cmd := exec.Command(decodeCmd[0], decodeCmd[1:]...)
	return outputFile, cmd.Run()
}
