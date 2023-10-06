# shortener

> url shortener

## API

### Register
```http request
POST localhost:80/shorten
Content-Type: application/json

{
  "url": "https://blog.equationzhao.space",
  "duration": 86400000000000
}
```

```json
{
  "shortened": "2wt8db"
}
```

duration: nanoseconds

### Get

```http request
GET localhost/2wt8db
```
302, redirect to https://blog.equationzhao.space

## Start

requirement:
- go >= 1.21

### build
```shell
go build
```
### running
```shell
./shortener -P 80 # set port at 80
```

## about

Those components were used to build this project

- [badger](https://github.com/dgraph-io/badger)
- [haxmap](https://github.com/alphadose/haxmap)
- [zap](https://github.com/uber-go/zap)
- [limiter](https://github.com/ulule/limiter/)
- [gin](https://github.com/gin-gonic/gin) and some middlewares
- [murmur3](https://github.com/spaolacci/murmur3)
- [pflag](https://github.com/spf13/pflag)

