package main

import (
	"encoding/json"
	"io/ioutil"
	"fmt"
	"os/exec"
	"bytes"
	"path"
	"strconv"
	"strings"
	"syscall"
	"log"
	"os"
	"os/signal"
	"sync"
)

type Config struct {
	Fuzz struct {
		InputFiles []string `json:"input-files"`
		OutputDir  string   `json:"output-dir"`
		Radamsa    string
	}
	Runner struct {
		Binary string
		Params []string
		Config struct {
			Template  string
			OutputDir string `json:"output-dir"`
		}
	}
	QueueLen int `json:"queue-len"`
}

func readConfig() Config {
	content, err := ioutil.ReadFile("fuzzer-conf.json")

	if err != nil {
		panic(err)
	}

	result := Config{}

	if e := json.Unmarshal(content, &result); e != nil {
		panic(e)
	}

	return result

}

func eval(orig string, values map[string]string) string {
	txt := orig
	for k, v := range values {
		old := "{{" + k + "}}"
		txt = strings.Replace(txt, old, v, -1)
	}
	return txt
}

func prepareConfig(config Config, values map[string]string, id int) string {

	content, err := ioutil.ReadFile(config.Runner.Config.Template)

	if err != nil {
		panic(err)
	}

	txt := eval(string(content), values)

	newFilename := path.Join(config.Runner.Config.OutputDir, fmt.Sprintf("%d.conf", id))
	ioutil.WriteFile(newFilename, []byte(txt), 0666)
	return newFilename
}

type WorkerResult struct {
	Id int
	Rc int
}

type Worker struct {
	quit   chan bool
	finished bool
	id     int
	job    chan int
	result chan WorkerResult
	config Config
	wg     sync.WaitGroup
}

func NewWorker(config Config, id int, job chan int, result chan WorkerResult) Worker {
	return Worker{
		quit:   make(chan bool),
		id:     id,
		result: result,
		config: config,
		job:    job,
		finished: false}
}

func run(config Config, id int, result chan WorkerResult) {

	// 1. make fuzzed input files
	var stdout, stderr bytes.Buffer

	values := make(map[string]string)
	values["worker_id"] = strconv.Itoa(id)
	for idx, inputFile := range config.Fuzz.InputFiles {
		baseName := path.Base(inputFile)
		target := path.Join(config.Fuzz.OutputDir, baseName+"."+strconv.Itoa(id))
		cmd := exec.Command(config.Fuzz.Radamsa, inputFile)
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			log.Printf("%d Fuzz failed: %v", id, err)
			result <- WorkerResult{Id: id, Rc: -1}
			return
		}

		ioutil.WriteFile(target, stdout.Bytes(), 0666)
		values [fmt.Sprintf("input_%d", idx)] = target
	}

	// parse template

	newConf := prepareConfig(config, values, id)

	values["config"] = newConf

	// fix params
	args := config.Runner.Params

	for idx := range args {
		args[idx] = eval(args[idx], values)
	}

	cmd := exec.Command(config.Runner.Binary, args...)
	log.Printf("#%d exec %v", id, config.Runner.Binary)
	cmd.Start()
	if err := cmd.Wait(); err != nil {

		if exiterr, ok := err.(*exec.ExitError); ok {
			// The program has exited with an exit code != 0

			// This works on both Unix and Windows. Although package
			// syscall is generally platform dependent, WaitStatus is
			// defined for both Unix and Windows and in both cases has
			// an ExitStatus() method with the same signature.
			if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				if status.ExitStatus() == 127 {
					log.Printf("SEGFAULTS")
					result <- WorkerResult{Id: id, Rc: status.ExitStatus()}
				}
				return
			}
		} else {
			log.Fatalf("cmd.Wait: %v", err)
		}
	}
	result <- WorkerResult{Id: id, Rc: 0}
}

func (w * Worker) Start() {

	w.wg.Add(1)
	log.Printf("Start worker %d", w.id)
	go func() {
		defer log.Printf("Stop worker %d", w.id)
		defer w.wg.Done()
		for !w.finished {
			log.Printf("Wait channel %d", w.id)
			select {
			case jobId := <-w.job:
				{
					log.Printf("Job signal for %d", w.id)
					run(w.config, jobId, w.result)
					log.Printf("Job done %d", w.id)
					if w.finished {
						log.Printf("Finish worker")
						return
					}
				}
			case <-w.quit:
				log.Printf("Quit signal for %d", w.id)
				w.finished = true
			}
		}
	}()
}

func (w * Worker) Stop() {
	log.Printf("Stopping worker %d:", w.id)
	go func() {
		w.quit <- true
	}()
	w.wg.Wait()
}

func main() {

	config := readConfig()
	result := make(chan WorkerResult)
	job := make(chan int)
	signals := make(chan os.Signal)
	quit := make(chan bool)

	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		sig := <-signals
		log.Printf("Caught sig: %+v\n", sig)
		quit <- true
	}()

	workers := make([]Worker, config.QueueLen)
	for i := range workers {
		workers[i] = NewWorker(config, i, job, result)
		workers[i].Start()
	}

	for i := 0; i < config.QueueLen; i ++ {
		job <- i
	}

	finished := false
	for {
		select {
		case rv := <-result:
			{
				if rv.Rc == 127 { // SEGFAULT
					go func() {
						quit <- true
					}()
				} else {
					go func() {
						job <- rv.Id
					}()
				}
			}
		case <-quit:
			log.Printf("Should exit")
			finished = true
		}

		if finished {
			break
		}
	}

	for i := 0; i < config.QueueLen; i ++ {
		workers[i].Stop()
	}

}
