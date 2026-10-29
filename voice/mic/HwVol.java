import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.lang.reflect.Method;

/**
 * Sets the station's hardware (amplifier) volume through the Amlogic audio HAL:
 * AudioSystem.setParameters("hw_master_volume=N"), N in 0..100. The HAL then
 * writes the TAS5825 "Tweeters DSP Playback Volume" mixer control.
 * Run as root via app_process (see hwvol.sh).
 *
 *   HwVol N   sets N once and prints the value read back.
 *   HwVol -   stays up and sets each N read from stdin, one per line, so a
 *             caller can ramp the volume without starting a VM per step.
 */
public class HwVol {
    static Method set, get;

    public static void main(String[] args) throws Exception {
        if (args.length != 1) {
            System.err.println("usage: HwVol <0-100>|-");
            System.exit(2);
        }
        Class<?> as = Class.forName("android.media.AudioSystem");
        set = as.getMethod("setParameters", String.class);
        get = as.getMethod("getParameters", String.class);
        if (!args[0].equals("-")) {
            System.exit(apply(args[0]) ? 0 : 1);
        }
        BufferedReader in = new BufferedReader(new InputStreamReader(System.in));
        for (String line; (line = in.readLine()) != null; ) {
            apply(line.trim());
        }
    }

    static boolean apply(String arg) throws Exception {
        int v;
        try {
            v = Integer.parseInt(arg);
        } catch (NumberFormatException e) {
            v = -1;
        }
        if (v < 0 || v > 100) {
            System.err.println("volume out of range: " + arg);
            return false;
        }
        int rc = (Integer) set.invoke(null, "hw_master_volume=" + v);
        String back = (String) get.invoke(null, "hw_master_volume");
        System.out.println(back);
        System.out.flush();
        if (rc != 0 || !back.equals("hw_master_volume=" + v)) {
            System.err.println("setParameters rc=" + rc);
            return false;
        }
        return true;
    }
}
