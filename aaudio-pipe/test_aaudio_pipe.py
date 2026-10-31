"""Host-side wire/format regression tests; fake only the Android AAudio device."""
import pathlib
import shutil
import struct
import subprocess
import sys
import tempfile
import unittest

HEADER = r'''
#include <stdint.h>
typedef struct {} AAudioStream;
typedef struct {} AAudioStreamBuilder;
typedef int aaudio_result_t;
#define AAUDIO_OK 0
#define AAUDIO_DIRECTION_OUTPUT 0
#define AAUDIO_SHARING_MODE_SHARED 0
#define AAUDIO_PERFORMANCE_MODE_NONE 0
#define AAUDIO_PERFORMANCE_MODE_LOW_LATENCY 1
#define AAUDIO_FORMAT_PCM_FLOAT 0
#define AAUDIO_USAGE_MEDIA 0
#define AAUDIO_CONTENT_TYPE_MUSIC 0
#define AAUDIO_CONTENT_TYPE_SPEECH 1
int AAudio_createStreamBuilder(AAudioStreamBuilder **);
void AAudioStreamBuilder_delete(AAudioStreamBuilder *);
const char *AAudio_convertResultToText(int);
#define SETTER(name) void AAudioStreamBuilder_set##name(AAudioStreamBuilder *, int);
SETTER(Direction) SETTER(SharingMode) SETTER(PerformanceMode) SETTER(Format)
SETTER(ChannelCount) SETTER(SampleRate) SETTER(Usage) SETTER(ContentType)
SETTER(BufferCapacityInFrames)
#undef SETTER
int AAudioStreamBuilder_openStream(AAudioStreamBuilder *, AAudioStream **);
int AAudioStream_setBufferSizeInFrames(AAudioStream *, int);
int AAudioStream_getBufferCapacityInFrames(AAudioStream *);
int AAudioStream_getSampleRate(AAudioStream *);
int AAudioStream_getFramesPerBurst(AAudioStream *);
int AAudioStream_getBufferSizeInFrames(AAudioStream *);
int AAudioStream_getXRunCount(AAudioStream *);
int AAudioStream_requestStart(AAudioStream *);
int AAudioStream_requestPause(AAudioStream *);
int AAudioStream_close(AAudioStream *);
int AAudioStream_write(AAudioStream *, const void *, int, int64_t);
'''
STUB = r'''
#include "aaudio/AAudio.h"
#include <stdio.h>
static AAudioStream stream;
static AAudioStreamBuilder builder;
static int rate, capacity;
int AAudio_createStreamBuilder(AAudioStreamBuilder **b) {*b=&builder;return 0;}
void AAudioStreamBuilder_delete(AAudioStreamBuilder *b) {}
const char *AAudio_convertResultToText(int x) {return "stub";}
#define SETTER(name) void AAudioStreamBuilder_set##name(AAudioStreamBuilder *b,int x) {}
SETTER(Direction) SETTER(SharingMode) SETTER(PerformanceMode) SETTER(Format)
SETTER(ChannelCount) SETTER(Usage) SETTER(ContentType)
void AAudioStreamBuilder_setSampleRate(AAudioStreamBuilder *b,int x) {rate=x;}
void AAudioStreamBuilder_setBufferCapacityInFrames(AAudioStreamBuilder *b,int x) {capacity=x;}
int AAudioStreamBuilder_openStream(AAudioStreamBuilder *b,AAudioStream **s) {*s=&stream;return 0;}
int AAudioStream_setBufferSizeInFrames(AAudioStream *s,int x) {return x;}
int AAudioStream_getBufferCapacityInFrames(AAudioStream *s) {return capacity;}
int AAudioStream_getSampleRate(AAudioStream *s) {return rate;}
int AAudioStream_getFramesPerBurst(AAudioStream *s) {return 1;}
int AAudioStream_getBufferSizeInFrames(AAudioStream *s) {return capacity;}
int AAudioStream_getXRunCount(AAudioStream *s) {return 0;}
int AAudioStream_requestStart(AAudioStream *s) {return 0;}
int AAudioStream_requestPause(AAudioStream *s) {return 0;}
int AAudioStream_close(AAudioStream *s) {return 0;}
int AAudioStream_write(AAudioStream *s,const void *p,int n,int64_t t) {fwrite(p,8,n,stdout);return n;}
'''

class AudioPipeTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.tmp.cleanup)
        root = pathlib.Path(cls.tmp.name)
        (root / "aaudio").mkdir()
        (root / "aaudio/AAudio.h").write_text(HEADER)
        (root / "stub.c").write_text(STUB)
        cls.exe = root / "pipe"
        flags = ["-Dst_mtim=st_mtimespec", "-DF_SETPIPE_SZ=1031"] if sys.platform == "darwin" else []
        subprocess.run([shutil.which("cc"), *flags, "-I", str(root), str(pathlib.Path(__file__).with_name("aaudio_pipe.c")), str(root / "stub.c"), "-o", str(cls.exe)], check=True)

    def test_speech_converts_mono_pcm16_to_stereo_float_without_music_gain(self):
        result = subprocess.run([self.exe, "--speech"], input=struct.pack("<hhh", -32768, 0, 16384), capture_output=True, check=True)
        self.assertEqual(struct.unpack("<ffffff", result.stdout), (-1., -1., 0., 0., .5, .5))
        self.assertIn(b"rate=24000", result.stderr)
        self.assertIn(b"buffer=960", result.stderr)

    def test_music_format_and_gain_are_unchanged(self):
        gain = pathlib.Path(self.tmp.name) / "gain"
        gain.write_text("2")
        result = subprocess.run([self.exe, "44100", gain], input=struct.pack("<ff", .25, -.75), capture_output=True, check=True)
        self.assertEqual(struct.unpack("<ff", result.stdout), (.5, -1.))
        self.assertIn(b"rate=44100", result.stderr)
        self.assertIn(b"buffer=8192", result.stderr)

    def test_speech_rejects_truncated_pcm(self):
        result = subprocess.run([self.exe, "--speech"], input=b"\x00", capture_output=True)
        self.assertNotEqual(result.returncode, 0)

if __name__ == "__main__":
    unittest.main()
