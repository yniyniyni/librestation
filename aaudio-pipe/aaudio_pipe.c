// aaudio_pipe: reads raw S16LE stereo PCM from stdin and plays it via AAudio.
// Usage: aaudio_pipe [sample_rate]   (default 44100, what librespot's pipe backend emits)
// The stream is paused after a short silence so the speaker does not keep
// the output path busy while Spotify is paused, and reopened if audioserver
// disconnects it (e.g. audioserver restart).

#include <aaudio/AAudio.h>
#include <errno.h>
#include <poll.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#define CHANNELS 2
#define FRAME_BYTES (CHANNELS * 2)
#define CHUNK_FRAMES 1024
#define IDLE_PAUSE_MS 1500
#define WRITE_TIMEOUT_NS (500LL * 1000 * 1000)

static int sample_rate = 44100;
static AAudioStream *stream;
static int playing;

static void close_stream(void) {
    if (stream) {
        AAudioStream_close(stream);
        stream = NULL;
    }
    playing = 0;
}

static int open_stream(void) {
    AAudioStreamBuilder *b;
    aaudio_result_t r = AAudio_createStreamBuilder(&b);
    if (r != AAUDIO_OK) {
        fprintf(stderr, "aaudio_pipe: createStreamBuilder: %s\n", AAudio_convertResultToText(r));
        return -1;
    }
    AAudioStreamBuilder_setDirection(b, AAUDIO_DIRECTION_OUTPUT);
    AAudioStreamBuilder_setSharingMode(b, AAUDIO_SHARING_MODE_SHARED);
    AAudioStreamBuilder_setPerformanceMode(b, AAUDIO_PERFORMANCE_MODE_NONE);
    AAudioStreamBuilder_setFormat(b, AAUDIO_FORMAT_PCM_I16);
    AAudioStreamBuilder_setChannelCount(b, CHANNELS);
    AAudioStreamBuilder_setSampleRate(b, sample_rate);
    AAudioStreamBuilder_setUsage(b, AAUDIO_USAGE_MEDIA);
    AAudioStreamBuilder_setContentType(b, AAUDIO_CONTENT_TYPE_MUSIC);
    r = AAudioStreamBuilder_openStream(b, &stream);
    AAudioStreamBuilder_delete(b);
    if (r != AAUDIO_OK) {
        fprintf(stderr, "aaudio_pipe: openStream: %s\n", AAudio_convertResultToText(r));
        stream = NULL;
        return -1;
    }
    fprintf(stderr, "aaudio_pipe: stream open, rate=%d burst=%d\n",
            AAudioStream_getSampleRate(stream), AAudioStream_getFramesPerBurst(stream));
    return 0;
}

static int ensure_playing(void) {
    if (!stream && open_stream() != 0)
        return -1;
    if (!playing) {
        aaudio_result_t r = AAudioStream_requestStart(stream);
        if (r != AAUDIO_OK) {
            fprintf(stderr, "aaudio_pipe: requestStart: %s\n", AAudio_convertResultToText(r));
            close_stream();
            return -1;
        }
        playing = 1;
    }
    return 0;
}

static void pause_stream(void) {
    if (stream && playing) {
        AAudioStream_requestPause(stream);
        playing = 0;
    }
}

static void play(const unsigned char *buf, int frames) {
    while (frames > 0) {
        if (ensure_playing() != 0) {
            // audioserver not ready; drop this chunk rather than block librespot forever
            usleep(200 * 1000);
            return;
        }
        aaudio_result_t n = AAudioStream_write(stream, buf, frames, WRITE_TIMEOUT_NS);
        if (n < 0) {
            fprintf(stderr, "aaudio_pipe: write: %s, reopening\n", AAudio_convertResultToText(n));
            close_stream();
            continue;
        }
        buf += n * FRAME_BYTES;
        frames -= n;
    }
}

int main(int argc, char **argv) {
    if (argc > 1)
        sample_rate = atoi(argv[1]);

    static unsigned char buf[CHUNK_FRAMES * FRAME_BYTES];
    size_t have = 0;
    struct pollfd pfd = {.fd = STDIN_FILENO, .events = POLLIN};

    for (;;) {
        int pr = poll(&pfd, 1, IDLE_PAUSE_MS);
        if (pr < 0) {
            if (errno == EINTR)
                continue;
            perror("aaudio_pipe: poll");
            break;
        }
        if (pr == 0) {
            pause_stream();
            continue;
        }
        ssize_t n = read(STDIN_FILENO, buf + have, sizeof(buf) - have);
        if (n < 0) {
            if (errno == EINTR)
                continue;
            perror("aaudio_pipe: read");
            break;
        }
        if (n == 0)
            break;
        have += n;
        size_t whole = have - have % FRAME_BYTES;
        if (whole) {
            play(buf, whole / FRAME_BYTES);
            memmove(buf, buf + whole, have - whole);
            have -= whole;
        }
    }
    close_stream();
    return 0;
}
