//go:generate picopacker goshove.glade glade.go glade
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/gotk3/gotk3/glib"

	"github.com/gotk3/gotk3/gdk"

	libgoshove "github.com/delthas/goshove"

	"github.com/gotk3/gotk3/gtk"
)

var builder *gtk.Builder
var logBuffer *gtk.TextBuffer
var fileSend *gtk.Button

func main() {
	runtime.LockOSThread()

	gtk.Init(nil)

	var err error
	builder, err = gtk.BuilderNew()
	if err != nil {
		panic(err)
	}
	if err := builder.AddFromString(string(glade)); err != nil {
		panic(err)
	}

	logBuffer = getTextBuffer("log-buffer")
	fileChooser := getFileChooserButton("file-chooser")
	fileSend = getButton("file-send")
	addressEntry := getEntry("address-entry")
	connect := getDialog("dialog-connect")
	incoming := getDialog("dialog-incoming")
	ipDialog := getMessageDialog("dialog-ip")
	fileName := getLabel("file-name")
	fileSize := getLabel("file-size")
	progressSend := getProgressBar("progressSend")
	progressReceive := getProgressBar("progressReceive")
	win := getWindow("window")

	var c *libgoshove.Conn

	peerAddressStr := ""
	res := connect.Run()
	if res == -2 {
		peerAddressStr, err = addressEntry.GetText()
		if err != nil {
			panic(err)
		}
		peerAddressStr = strings.TrimSpace(peerAddressStr)
	}
	connect.Destroy()

	go func() {
		if res == -2 {
			glib.IdleAdd(func() {
				win.ShowAll()
			})
			c, err = libgoshove.Dial(peerAddressStr, &libgoshove.ConnSettings{
				ReadTimeout: 5 * time.Second,
			})
			if err != nil {
				panic(err)
			}
		} else if res == -1 {
			c, err = libgoshove.Accept(&libgoshove.AcceptSettings{
				ConnSettings: libgoshove.ConnSettings{
					ReadTimeout: 5 * time.Second,
				},
				ListenCallback: func(ip string, port int) {
					glib.IdleAdd(func() {
						setClipboard(fmt.Sprintf("%s:%d", ip, port))
						logf("IP: %s:%d\n", ip, port)
						_ = ipDialog.Run()
						ipDialog.Destroy()
						win.ShowAll()
					})
				},
			})
			if err != nil {
				panic(err)
			}
		}

		glib.IdleAdd(func() {
			fileSend.SetSensitive(true)
			fileSend.Connect("clicked", func(_ *gtk.Button) {
				file := fileChooser.GetFilename()
				if file == "" {
					return
				}
				go func() {
					fi, err := os.Stat(file)
					if err != nil {
						panic(err)
					}
					f, err := os.Open(file)
					if err != nil {
						panic(err)
					}
					defer f.Close()
					err = c.Send(file, fi.Size(), f, func(totalSent int64, bandwidth int64) {
						glib.IdleAdd(func() {
							progressSend.SetText(fmt.Sprintf("upload speed: %5dko/s\n", bandwidth/1000))
							progressSend.SetFraction(float64(totalSent) / float64(fi.Size()))
						})
					})
					if err != nil {
						logf("upload failed for %s: %s\n", file, err.Error())
					} else {
						logf("upload finished: %s\n", file)
					}
				}()
			})
		})

		for offer := range c.Offers {
			resCh := make(chan gtk.ResponseType)
			glib.IdleAdd(func() {
				fileName.SetText(offer.Name)
				fileSize.SetText(fmt.Sprintf("%d kio", offer.Size/1024))
				res := incoming.Run()
				incoming.Hide()
				resCh <- res
			})
			res := <-resCh
			if res != gtk.RESPONSE_ACCEPT {
				offer.Reject()
				continue
			}
			f, err := os.OpenFile(offer.Name, os.O_TRUNC|os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				panic(err)
			}
			if err := f.Truncate(offer.Size); err != nil {
				panic(err)
			}
			err = offer.Accept(f, func(totalReceived int64, bandwidth int64) {
				glib.IdleAdd(func() {
					progressReceive.SetText(fmt.Sprintf("download speed: %5dko/s\n", bandwidth/1000))
					progressReceive.SetFraction(float64(totalReceived) / float64(offer.Size))
				})
			})
			f.Close()
			if err != nil {
				logf("download failed for %s: %s\n", offer.Name, err.Error())
			} else {
				logf("download finished: %s\n", offer.Name)
			}
		}
		os.Exit(1)
	}()

	win.Connect("destroy", func() {
		gtk.MainQuit()
	})

	gtk.Main()
}

func getProgressBar(name string) *gtk.ProgressBar {
	v, err := builder.GetObject(name)
	if err != nil {
		panic(err)
	}
	return v.(*gtk.ProgressBar)
}

func getTextBuffer(name string) *gtk.TextBuffer {
	v, err := builder.GetObject(name)
	if err != nil {
		panic(err)
	}
	return v.(*gtk.TextBuffer)
}

func getButton(name string) *gtk.Button {
	v, err := builder.GetObject(name)
	if err != nil {
		panic(err)
	}
	return v.(*gtk.Button)
}

func getLabel(name string) *gtk.Label {
	v, err := builder.GetObject(name)
	if err != nil {
		panic(err)
	}
	return v.(*gtk.Label)
}

func getFileChooserButton(name string) *gtk.FileChooserButton {
	v, err := builder.GetObject(name)
	if err != nil {
		panic(err)
	}
	return v.(*gtk.FileChooserButton)
}

func getEntry(name string) *gtk.Entry {
	v, err := builder.GetObject(name)
	if err != nil {
		panic(err)
	}
	return v.(*gtk.Entry)
}

func getDialog(name string) *gtk.Dialog {
	v, err := builder.GetObject(name)
	if err != nil {
		panic(err)
	}
	return v.(*gtk.Dialog)
}

func getMessageDialog(name string) *gtk.MessageDialog {
	v, err := builder.GetObject(name)
	if err != nil {
		panic(err)
	}
	return v.(*gtk.MessageDialog)
}

func getWindow(name string) *gtk.Window {
	v, err := builder.GetObject(name)
	if err != nil {
		panic(err)
	}
	return v.(*gtk.Window)
}

func setClipboard(text string) {
	clipboard, _ := gtk.ClipboardGet(gdk.GdkAtomIntern("CLIPBOARD", false))
	clipboard.SetText(text)
}

func logf(format string, args ...interface{}) {
	glib.IdleAdd(func() {
		logBuffer.Insert(logBuffer.GetEndIter(), fmt.Sprintf(format, args...))
	})
}
