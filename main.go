package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"time"

	"github.com/alphadose/haxmap"
	"github.com/dgraph-io/badger/v4"
	"github.com/gin-contrib/gzip"
	ginzap "github.com/gin-contrib/zap"
	"github.com/gin-gonic/gin"
	"github.com/spaolacci/murmur3"
	"github.com/spf13/pflag"
	"github.com/ulule/limiter/v3"
	m "github.com/ulule/limiter/v3/drivers/middleware/gin"
	"github.com/ulule/limiter/v3/drivers/store/memory"
	"go.uber.org/zap"
)

func init() {
	l, _ := zap.NewProduction()
	zap.ReplaceGlobals(l)
	db = getDB()
}

type toShortened struct {
	Url      string        `json:"url"`
	Duration time.Duration `json:"duration"`
}

type toStore struct {
	Url       string
	ExpiredAt uint64
}

var db *badger.DB

func getDB() *badger.DB {
	db, err := badger.Open(badger.DefaultOptions("./db").WithLoggingLevel(badger.ERROR))
	if err != nil {
		log.Fatal(err)
	}
	return db
}

func loadDB() *haxmap.Map[string, toStore] {
	mp := haxmap.New[string, toStore](8)
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
	port := pflag.StringP("port", "P", "80", "running port")
	pflag.Parse()

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
				Url:       urlToStore,
				ExpiredAt: expiration,
			})
			if !loaded { // not exist and insert
				err := db.Update(func(txn *badger.Txn) error {
					e := badger.NewEntry([]byte(res), []byte(urlToStore))
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
	ginzap.RecoveryWithZap(zap.L(), true)
	app.Use(gzip.Gzip(gzip.DefaultCompression))
	rate, _ := limiter.NewRateFromFormatted("10-M")
	app.POST("/shorten", m.NewMiddleware(limiter.New(memory.NewStore(), rate)),
		func(c *gin.Context) {
			toShort := toShortened{}
			err := c.BindJSON(&toShort)
			if err != nil {
				c.JSONP(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
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

	app.GET("/:hash", func(c *gin.Context) {
		hash := c.Param("hash")
		url, ok := mp.Get(hash)
		if !ok {
			c.JSONP(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		if url.ExpiredAt != 0 && url.ExpiredAt < uint64(time.Now().Unix()) {
			mp.Del(hash)
			c.JSONP(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.Redirect(http.StatusFound, url.Url)
	})

	srv := &http.Server{
		Addr:    ":" + *port,
		Handler: app,
	}
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)

	go func() {
		err := srv.ListenAndServe()
		if err != nil {
			zap.L().Error("error running", zap.Error(err))
		}
	}()

	<-quit
	zap.L().Info("Shutdown Server ...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		zap.L().Fatal("Server Shutdown:", zap.Error(err))
	}
	zap.L().Info("Server exiting")
}
