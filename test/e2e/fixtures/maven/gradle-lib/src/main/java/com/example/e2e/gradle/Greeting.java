package com.example.e2e.gradle;

/** The class of the library that the Lajstrom end-to-end scenario publishes with Gradle. */
public final class Greeting {
    private Greeting() {}

    /**
     * Returns the greeting of the library.
     *
     * @return the greeting
     */
    public static String text() {
        return "hello from the Lajstrom e2e Gradle lib";
    }
}
