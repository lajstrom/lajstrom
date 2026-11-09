package com.example.e2e;

/** The class of the library that the Lajstrom end-to-end scenario deploys with mvn deploy. */
public final class Hello {
    private Hello() {}

    /**
     * Returns the greeting of the library.
     *
     * @return the greeting
     */
    public static String greeting() {
        return "hello from the Lajstrom e2e lib";
    }
}
