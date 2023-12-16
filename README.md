# World Radio

Record up to 10 second sound bytes and push them to an RTMP server

### Dependencies:
 - [ffmpeg](https://ffmpeg.org/)

### Steps:

1. Build: `go build -o wrserver main.go`
2. Start local rtmp server: `docker run -d -p 1935:1935 --name nginx-rtmp tiangolo/nginx-rtmp`
3. Start WR server: `./wrserver`
4. Record: http://localhost:8080
5. Listen: rtmp://localhost/live/stream

### References:
 * nginx-rtmp docker server [[link](https://github.com/tiangolo/nginx-rtmp-docker)]
 * ffmpeg [[link](https://ffmpeg.org)]
 * vlc for listening [[link](https://www.videolan.org/vlc/)]
 * rtmp protocol specification [[link](https://rtmp.veriskope.com/docs/spec/)]