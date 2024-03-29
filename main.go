package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/alphadose/haxmap"
	"github.com/dgraph-io/badger/v4"
	"github.com/gin-contrib/gzip"
	"github.com/gin-contrib/pprof"
	ginzap "github.com/gin-contrib/zap"
	"github.com/gin-gonic/gin"
	"github.com/spaolacci/murmur3"
	"github.com/ulule/limiter/v3"
	m "github.com/ulule/limiter/v3/drivers/middleware/gin"
	"github.com/ulule/limiter/v3/drivers/store/memory"
	"github.com/yeqown/go-qrcode/v2"
	"github.com/yeqown/go-qrcode/writer/standard"
	"go.uber.org/zap"
)

func init() {
	l, _ := zap.NewProduction()
	zap.ReplaceGlobals(l)
	userConfigDie, err := os.UserConfigDir()
	if err != nil {
		zap.L().Fatal("failed to get user config dir", zap.Error(err))
	}
	configFile, err := os.ReadFile(filepath.Join(userConfigDie, "shortener", "config.yaml"))
	if err != nil {
		zap.L().Fatal("failed to read config", zap.Error(err))
	}
	err = loadConfig(configFile)
	if err != nil {
		zap.L().Fatal("failed to load config", zap.Error(err))
	}
	db = getDB(Config.DBPath)
}

type toShortened struct {
	Shortened string        `json:"shortened"`
	Url       string        `json:"url"`
	Duration  time.Duration `json:"duration"`
}

type toStore struct {
	Url       string
	ExpiredAt uint64
}

var db *badger.DB

func getDB(path string) *badger.DB {
	db, err := badger.Open(badger.DefaultOptions(path).WithLoggingLevel(badger.ERROR))
	if err != nil {
		zap.L().Fatal("failed to open db", zap.Error(err))
	}
	return db
}

func loadDB() *haxmap.Map[string, toStore] {
	zap.L().Info("loading db...")
	mp := haxmap.New[string, toStore](uintptr(Config.CacheInitializationSize))
	err := db.View(func(txn *badger.Txn) error {
		defer txn.Commit()
		iterator := txn.NewIterator(badger.DefaultIteratorOptions)
		defer iterator.Close()
		for iterator.Rewind(); iterator.Valid(); iterator.Next() {
			item := iterator.Item()
			valCopy := make([]byte, 0, 10)
			var err error
			valCopy, err = item.ValueCopy(valCopy)
			if err != nil {
				return err
			}
			s := toStore{
				Url:       string(valCopy),
				ExpiredAt: item.ExpiresAt(),
			}
			mp.Set(string(item.Key()), s)
		}
		return nil
	})
	if err != nil {
		zap.L().Fatal("failed to load db", zap.Error(err))
	}
	return mp
}

var ErrAlreadyExist = errors.New("already exist")

var map62 = map[uint32]byte{
	0: '0', 1: '1', 2: '2', 3: '3', 4: '4', 5: '5', 6: '6', 7: '7', 8: '8', 9: '9',
	10: 'a', 11: 'b', 12: 'c', 13: 'd', 14: 'e', 15: 'f', 16: 'g', 17: 'h', 18: 'i', 19: 'j', 20: 'k', 21: 'l', 22: 'm', 23: 'n', 24: 'o', 25: 'p', 26: '1', 27: 'r', 28: 's', 29: 't', 30: 'u', 31: 'v', 32: 'w', 33: 'x', 34: 'y', 35: 'z',
	36: 'a', 37: 'b', 38: 'c', 39: 'd', 40: 'e', 41: 'f', 42: 'g', 43: 'h', 44: 'i', 45: 'j', 46: 'k', 47: 'l', 48: 'm', 49: 'n', 50: 'o', 51: 'p', 52: '1', 53: 'r', 54: 's', 55: 't', 56: 'u', 57: 'v', 58: 'w', 59: 'x', 60: 'y', 61: 'z',
}

func main() {
	port := Config.Port
	zap.L().Info("running port", zap.Uint16("port", port))
	mp := loadDB()
	defer db.Close()

	maps := func(a uint32) string {
		b := bytes.Buffer{}
		for {
			if a == 0 {
				break
			}
			i := a % 62
			left := a / 62
			b.WriteByte(map62[i])
			a = left
		}
		res := b.Bytes()
		slices.Reverse(res)
		return string(res)
	}

	hashingAndStore := func(url string, duration time.Duration) (string, error) {
		urlToStore := url
		for {
			sum32 := murmur3.Sum32WithSeed([]byte(urlToStore), 0)
			res := maps(sum32)
			expiration := uint64(0)
			if duration != 0 {
				expiration = uint64(time.Now().Add(duration).Unix())
			}
			v, loaded := mp.GetOrSet(res, toStore{
				Url:       url,
				ExpiredAt: expiration,
			})
			if !loaded { // not exist and insert
				err := db.Update(func(txn *badger.Txn) error {
					e := badger.NewEntry([]byte(res), []byte(url))
					if duration > 0 {
						e = e.WithTTL(duration)
					}
					return txn.SetEntry(e)
				})
				if err != nil {
					return "", err
				}
				return res, nil
			}
			// exist
			if v.Url == url {
				return res, ErrAlreadyExist
			}
			urlToStore = urlToStore + "3.14159"
		}
	}

	gin.SetMode(gin.ReleaseMode)
	app := gin.New()
	app.Use(ginzap.Ginzap(zap.L(), time.RFC3339, true))
	app.Use(gzip.Gzip(gzip.DefaultCompression))
	rate, _ := limiter.NewRateFromFormatted(Config.IPlimit)
	app.POST("/shorten", m.NewMiddleware(limiter.New(memory.NewStore(), rate)),
		func(c *gin.Context) {
			toShort := toShortened{}
			err := c.BindJSON(&toShort)
			if err != nil {
				c.JSONP(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}

			if toShort.Shortened != "" {
				expiration := uint64(0)
				if toShort.Duration != 0 {
					expiration = uint64(time.Now().Add(toShort.Duration).Unix())
				}

				_, loaded := mp.GetOrSet(toShort.Shortened, toStore{
					Url:       toShort.Url,
					ExpiredAt: expiration,
				})
				if loaded {
					c.JSONP(http.StatusAccepted, gin.H{"error": ErrAlreadyExist.Error()})
					return
				} else {
					err := db.Update(func(txn *badger.Txn) error {
						e := badger.NewEntry([]byte(toShort.Shortened), []byte(toShort.Url))
						if toShort.Duration > 0 {
							e = e.WithTTL(toShort.Duration)
						}
						return txn.SetEntry(e)
					})
					if err != nil {
						c.JSONP(http.StatusInternalServerError, gin.H{"error": err.Error()})
						return
					}
					c.JSONP(http.StatusOK, gin.H{"shortened": toShort.Shortened})
					return
				}
			}
			hash, err := hashingAndStore(toShort.Url, toShort.Duration)
			if err != nil {
				if errors.Is(err, ErrAlreadyExist) {
					c.JSONP(http.StatusAccepted, gin.H{"error": err.Error(), "shortened": hash})
					return
				}
				c.JSONP(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.JSONP(http.StatusOK, gin.H{"shortened": hash})
		})

	app.GET("/", func(c *gin.Context) {
		code, ok := c.GetQuery("code")
		if !ok {
			c.JSON(http.StatusOK, mp)
			return
		}
		c.Redirect(http.StatusFound, "/"+code)
	})

	app.GET("/:code", func(c *gin.Context) {
		code := c.Param("code")
		url, ok := mp.Get(code)
		if !ok {
			c.JSONP(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		if url.ExpiredAt != 0 && url.ExpiredAt < uint64(time.Now().Unix()) {
			mp.Del(code)
			c.JSONP(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.Redirect(http.StatusFound, url.Url)
	})

	pprof.Register(app)

	app.POST("/qr/:code", func(c *gin.Context) {
		code := c.Param("code")
		url, ok := mp.Get(code)
		if !ok {
			c.JSONP(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		if url.ExpiredAt != 0 && url.ExpiredAt < uint64(time.Now().Unix()) {
			mp.Del(code)
			c.JSONP(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		qrc, err := qrcode.NewWith(url.Url, qrcode.WithEncodingMode(qrcode.EncModeByte))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		var w io.WriteCloser = &WC{
			bufio.NewWriter(c.Writer),
		}
		qrc.Save(standard.NewWithWriter(w, standard.WithBgTransparent(), standard.WithQRWidth(10)))
	})

	srv := &http.Server{
		Addr:    ":" + strconv.Itoa(int(port)),
		Handler: app,
	}
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	// clean map
	if Config.CleanInterval != 0 {
		toDelSize := int(max(Config.CleanBatchSize, 1000))
		zap.L().Info("start clean map...", zap.Uint16("interval(minute)", Config.CleanInterval), zap.Int("batch size", toDelSize))
		go func() {
			defer func() {
				if err := recover(); err != nil {
					zap.L().Error("clean map panic", zap.Any("err", err))
				}
			}()
			for {
				time.Sleep(time.Minute * time.Duration(Config.CleanInterval))
				zap.L().Info("clean map start")
				toDel := make([]string, 0, toDelSize)
				mp.ForEach(func(s string, store toStore) bool {
					if len(toDel) >= toDelSize {
						return false
					}
					if store.ExpiredAt != 0 && store.ExpiredAt < uint64(time.Now().Unix()) {
						toDel = append(toDel, s)
					}
					return true
				})
				zap.L().Info("clean map", zap.Int("count", len(toDel)))
				mp.Del(toDel...)
			}
		}()
	}

	go func() {
		err := srv.ListenAndServe()
		if err != http.ErrServerClosed {
			zap.L().Error("error running", zap.Error(err))
		}
	}()

	<-quit
	zap.L().Info("Shutdown Server ...")
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(Config.ShutdownTimeout)*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		zap.L().Fatal("Server Shutdown:", zap.Error(err))
	}
	zap.L().Info("Server exiting")
}

type WC struct {
	*bufio.Writer
}

func (wc *WC) Close() error {
	return wc.Flush()
}
