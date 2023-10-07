wrk -t8 -c160 -d30s --latency http://localhost:80/sf5oy &
curl -o new.pgo 'http://localhost:80/debug/pprof/profile?seconds=30'
go tool pprof -proto default.pgo new.pgo > default-new.pgo
rm new.pgo
mv default-new.pgo default.pgo