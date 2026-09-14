plugins {
    kotlin("jvm") version "2.4.20"
    id("com.example.included")
}

repositories {
    mavenCentral()
}

dependencies {
    testImplementation(libs.kotlin.test)
}

tasks.test {
    useJUnitPlatform()
}
