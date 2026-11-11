import java.lang.reflect.Method;

/**
 * Sets the Amlogic audio HAL's EQ mode: AudioSystem.setParameters("eq_mode=N").
 * Run as root via app_process (see eq.sh).
 *
 *   EqMode N   sets mode N (0-9) once.
 */
public class EqMode {
    public static void main(String[] args) throws Exception {
        if (args.length != 1) {
            System.err.println("usage: EqMode <0-9>");
            System.exit(2);
        }
        int mode;
        try {
            mode = Integer.parseInt(args[0]);
        } catch (NumberFormatException e) {
            System.err.println("bad mode: " + args[0]);
            System.exit(1);
            return;
        }
        if (mode < 0 || mode > 9) {
            System.err.println("mode out of range: " + mode);
            System.exit(1);
        }
        Class<?> as = Class.forName("android.media.AudioSystem");
        Method set = as.getMethod("setParameters", String.class);
        int rc = (Integer) set.invoke(null, "eq_mode=" + mode);
        if (rc != 0) {
            System.err.println("eq_mode=" + mode + " failed: rc=" + rc);
            System.exit(1);
        }
    }
}
