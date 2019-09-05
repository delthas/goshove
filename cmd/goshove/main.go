package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	libgoshove "github.com/delthas/goshove"
)

const usage = `goshove - send and receive files with custom aggressive UDP congestion control

usage: hosting:  ` + "`" + `goshove` + "`" + `
usage: connecting:  ` + "`" + `goshove -connect host:port file` + "`" + `

flags: connect [host:port] - connect to the specified address instead of hosting
flags: yes - accept all file offers automatically, defaults to false
flags: port [port] - port used for udp socket, defaults to 0
flags: help - show this help and exit

misc: author: delthas <delthas@dille.cc>
misc: license: MIT
`

func main() {
	address := flag.String("connect", "", "address to connect to")
	port := flag.Int("port", 0, "listen port")
	yes := flag.Bool("yes", false, "accept all file offers")
	help := flag.Bool("help", false, "show help")
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
	}
	flag.Parse()

	if *help {
		fmt.Fprint(os.Stderr, usage)
		return
	}

	err := func() error {
		var c *libgoshove.Conn
		var err error
		if *address != "" {
			name := flag.Arg(0)
			if name == "" {
				return errors.New("must specify a file to send when connecting to a peer")
			}
			fmt.Fprintf(os.Stderr, "connecting to: %s\n", *address)
			c, err = libgoshove.Dial(*address, &libgoshove.ConnSettings{
				ListenPort: *port,
			})
			if err != nil {
				return err
			}
			defer c.Close()
			fi, err := os.Stat(name)
			if err != nil {
				return err
			}
			f, err := os.Open(name)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "offering %s (%dkiB)\n", name, fi.Size())
			err = c.Send(filepath.Base(f.Name()), fi.Size(), f, func(totalSent int64, bandwidth int64) {
				fmt.Fprintf(os.Stderr, "\rupload progress: %2.2f%% (%dkiB downloaded of %d kiB) at %dkiB/s", 100*float64(totalSent)/float64(fi.Size()), totalSent, fi.Size(), bandwidth/1024)
			})
			fmt.Fprintf(os.Stderr, "\n")
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "uploaded %s successfully\n", name)
		} else {
			c, err = libgoshove.Accept(&libgoshove.AcceptSettings{
				ConnSettings: libgoshove.ConnSettings{
					ListenPort: *port,
				},
				ListenCallback: func(ip string, port int) {
					fmt.Fprintf(os.Stderr, "hosting, address: %s:%d\n", ip, port)
				},
			})
			if err != nil {
				return err
			}
			defer c.Close()
			sc := bufio.NewScanner(os.Stdin)
		outer:
			for offer := range c.Offers {
				fmt.Fprintf(os.Stderr, "received offer for file %s of size %dkiB\n", offer.Name, offer.Size/1024)
				if !*yes {
					for {
						fmt.Fprintf(os.Stderr, "accept the offer? [y]/n: \n")
						if !sc.Scan() {
							return io.EOF
						}
						switch strings.ToLower(sc.Text()) {
						case "reject", "r", "n", "no":
							fmt.Fprintf(os.Stderr, "offer rejected\n")
							offer.Reject()
							continue outer
						case "", "accept", "a", "y", "yes":
						default:
							continue
						}
						break
					}
				}
				fmt.Fprintf(os.Stderr, "offer accepted, starting download\n")
				f, err := os.OpenFile(offer.Name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
				if err != nil {
					return err
				}
				if err := f.Truncate(offer.Size); err != nil {
					return err
				}
				err = offer.Accept(f, func(totalReceived int64, bandwidth int64) {
					fmt.Fprintf(os.Stderr, "\rdownload progress: %2.2f%% (%dkiB downloaded of %d kiB) at %dkiB/s", 100*float64(totalReceived)/float64(offer.Size), totalReceived, offer.Size, bandwidth/1024)
				})
				f.Close()
				fmt.Fprintf(os.Stderr, "\n")
				if err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "downloaded %s successfully\n", offer.Name)
			}
		}
		return nil
	}()
	if err != nil {
		fmt.Fprintf(os.Stderr, err.Error())
		os.Exit(1)
	}
}
