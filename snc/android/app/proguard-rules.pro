-keep class com.shortnerdcat.snc.** { *; }
# Keep FileDescriptor.descriptor field (used via reflection to get raw fd int).
-keepclassmembers class java.io.FileDescriptor {
    int descriptor;
}
