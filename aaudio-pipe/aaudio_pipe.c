// aaudio_pipe: reads raw FLOAT32LE stereo PCM from stdin and plays it via AAudio.
// Usage: aaudio_pipe [sample_rate [gain_file]]   (default 44100)
// gain_file holds one number the samples are multiplied by; it is re-read when
// it changes. voiced writes there the inverse of Soloist's software volume, so
// the music leaves here at full level and the amplifier sets the loudness.
// The stream is paused after a short silence so the speaker does not keep
// the output path busy while Spotify is paused, and reopened if audioserver
// disconnects it (e.g. audioserver restart).

#define _GNU_SOURCE // F_SETPIPE_SZ
#include <aaudio/AAudio.h>
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>

#define CHANNELS 2
#define FRAME_BYTES (CHANNELS * 4)
#define CHUNK_FRAMES 1024
#define IDLE_PAUSE_MS 1500
#define WRITE_TIMEOUT_NS (500LL * 1000 * 1000)
#define GAIN_CHECK_NS (10LL * 1000 * 1000)
// The output buffer rides out scheduling hiccups (~185 ms); the FIFO in front
// of us is kept small so the gain stays in step with Soloist's own volume.
#define BUFFER_FRAMES 8192
#define FIFO_BYTES 4096
#define XRUN_LOG_NS (10LL * 1000 * 1000 * 1000)

static int sample_rate = 44100;
static const char *gain_file;
static float gain = 1.0f;
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
    AAudioStreamBuilder_setFormat(b, AAUDIO_FORMAT_PCM_FLOAT);
    AAudioStreamBuilder_setChannelCount(b, CHANNELS);
    AAudioStreamBuilder_setSampleRate(b, sample_rate);
    AAudioStreamBuilder_setUsage(b, AAUDIO_USAGE_MEDIA);
    AAudioStreamBuilder_setContentType(b, AAUDIO_CONTENT_TYPE_MUSIC);
    AAudioStreamBuilder_setBufferCapacityInFrames(b, BUFFER_FRAMES);
    r = AAudioStreamBuilder_openStream(b, &stream);
    AAudioStreamBuilder_delete(b);
    if (r != AAUDIO_OK) {
        fprintf(stderr, "aaudio_pipe: openStream: %s\n", AAudio_convertResultToText(r));
        stream = NULL;
        return -1;
    }
    AAudioStream_setBufferSizeInFrames(stream, AAudioStream_getBufferCapacityInFrames(stream));
    fprintf(stderr, "aaudio_pipe: stream open, rate=%d burst=%d buffer=%d\n",
            AAudioStream_getSampleRate(stream), AAudioStream_getFramesPerBurst(stream),
            AAudioStream_getBufferSizeInFrames(stream));
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

static long long now_ns(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return ts.tv_sec * 1000000000LL + ts.tv_nsec;
}

// update_gain re-reads gain_file at most every GAIN_CHECK_NS, when it changed.
static void update_gain(void) {
    static long long next;
    static struct timespec seen;
    struct stat st;
    long long t = now_ns();
    if (!gain_file || t < next)
        return;
    next = t + GAIN_CHECK_NS;
    if (stat(gain_file, &st) != 0 ||
        (st.st_mtim.tv_sec == seen.tv_sec && st.st_mtim.tv_nsec == seen.tv_nsec))
        return;
    seen = st.st_mtim;
    FILE *f = fopen(gain_file, "r");
    float g;
    if (f && fscanf(f, "%f", &g) == 1 && g >= 0 && g < 1e7f) {
        if (g != gain)
            fprintf(stderr, "aaudio_pipe: gain %g\n", g);
        gain = g;
    }
    if (f)
        fclose(f);
}

static void apply_gain(float *s, int n) {
    for (int i = 0; i < n; i++) {
        float v = s[i] * gain;
        s[i] = v > 1.0f ? 1.0f : v < -1.0f ? -1.0f : v;
    }
}

// log_xruns reports output underruns now and then, so dropouts show in the log.
static void log_xruns(void) {
    static long long next;
    static int32_t last;
    long long t = now_ns();
    if (!stream || t < next)
        return;
    next = t + XRUN_LOG_NS;
    int32_t x = AAudioStream_getXRunCount(stream);
    if (x > last)
        fprintf(stderr, "aaudio_pipe: %d underruns (+%d)\n", x, x - last);
    last = x;
}

static void play(unsigned char *buf, int frames) {
    update_gain();
    log_xruns();
    apply_gain((float *)buf, frames * CHANNELS);
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
    if (argc > 2)
        gain_file = argv[2];
    if (fcntl(STDIN_FILENO, F_SETPIPE_SZ, FIFO_BYTES) < 0)
        perror("aaudio_pipe: F_SETPIPE_SZ");

    static float fbuf[CHUNK_FRAMES * CHANNELS];
    unsigned char *buf = (unsigned char *)fbuf;
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
        ssize_t n = read(STDIN_FILENO, buf + have, sizeof(fbuf) - have);
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
