package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"gopkg.in/errgo.v1"
	"gopkg.in/schmorrison/conflux.v2/recon"
	"gopkg.in/schmorrison/hkp.v1/sks"
	"gopkg.in/schmorrison/hkp.v1/storage"
	"gopkg.in/schmorrison/openpgp.v1"
	"gopkg.in/tomb.v2"

	"github.com/schmorrison/server"
	"github.com/schmorrison/server/cmd"
)

var (
	configFile = flag.String("config", "", "config file")
	outputDir  = flag.String("path", ".", "output path")
	count      = flag.Int("count", 15000, "keys per file")
	cpuProf    = flag.Bool("cpuprof", false, "enable CPU profiling")
	memProf    = flag.Bool("memprof", false, "enable mem profiling")
)

func main() {
	flag.Parse()

	var (
		settings *server.Settings
		err      error
	)
	if configFile != nil {
		conf, err := ioutil.ReadFile(*configFile)
		if err != nil {
			cmd.Die(errgo.Mask(err))
		}
		settings, err = server.ParseSettings(string(conf))
		if err != nil {
			cmd.Die(errgo.Mask(err))
		}
	}

	cpuFile := cmd.StartCPUProf(*cpuProf, nil)

	c := make(chan os.Signal)
	signal.Notify(c, syscall.SIGUSR2)
	go func() {
		for {
			select {
			case sig := <-c:
				switch sig {
				case syscall.SIGUSR2:
					cpuFile = cmd.StartCPUProf(*cpuProf, cpuFile)
					cmd.WriteMemProf(*memProf)
				}
			}
		}
	}()

	err = dump(settings)
	cmd.Die(err)
}

func dump(settings *server.Settings) error {
	st, err := server.DialStorage(settings)
	if err != nil {
		return errgo.Mask(err)
	}
	defer st.Close()

	ptree, err := sks.NewPrefixTree(settings.Conflux.Recon.LevelDB.Path, &settings.Conflux.Recon.Settings)
	if err != nil {
		return errgo.Mask(err)
	}
	err = ptree.Create()
	if err != nil {
		return errgo.Mask(err)
	}
	defer ptree.Close()

	root, err := ptree.Root()
	if err != nil {
		return errgo.Mask(err)
	}

	var t tomb.Tomb
	ch := make(chan string)

	t.Go(func() error {
		var i int
		var digests []string
		defer func() {
			for _ = range ch {
			}
		}() // drain if early return on error
		for digest := range ch {
			digests = append(digests, digest)
			if len(digests) >= *count {
				err := writeKeys(st, digests, i)
				if err != nil {
					return errgo.Mask(err)
				}
				i++
				digests = nil
			}
		}
		if len(digests) > 0 {
			err := writeKeys(st, digests, i)
			if err != nil {
				return errgo.Mask(err)
			}
		}
		return nil
	})
	t.Go(func() error {
		return traverse(root, ch)
	})
	return t.Wait()
}

func traverse(root recon.PrefixNode, ch chan string) error {
	defer close(ch)
	// Depth-first walk of the prefix tree
	nodes := []recon.PrefixNode{root}
	for len(nodes) > 0 {
		node := nodes[0]
		nodes = nodes[1:]

		if node.IsLeaf() {
			elements, err := node.Elements()
			if err != nil {
				return errgo.Mask(err)
			}
			for _, element := range elements {
				zb := element.Bytes()
				ch <- strings.ToLower(hex.EncodeToString(zb))
			}
		} else {
			children, err := node.Children()
			if err != nil {
				return errgo.Mask(err)
			}
			nodes = append(nodes, children...)
		}
	}
	return nil
}

const chunksize = 20

func writeKeys(st storage.Queryer, digests []string, num int) error {
	rfps, err := st.MatchMD5(digests)
	if err != nil {
		return errgo.Mask(err)
	}
	log.Printf("matched %d fingerprints", len(rfps))
	f, err := os.Create(filepath.Join(*outputDir, fmt.Sprintf("hkp-dump-%04d.pgp", num)))
	if err != nil {
		return errgo.Mask(err)
	}
	defer f.Close()

	for len(rfps) > 0 {
		var chunk []string
		if len(rfps) > chunksize {
			chunk = rfps[:chunksize]
			rfps = rfps[chunksize:]
		} else {
			chunk = rfps
			rfps = nil
		}

		keys, err := st.FetchKeys(chunk)
		if err != nil {
			return errgo.Mask(err)
		}
		for _, key := range keys {
			err := openpgp.WritePackets(f, key)
			if err != nil {
				return errgo.Mask(err)
			}
		}
	}
	return nil
}
